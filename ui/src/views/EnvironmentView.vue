<script setup lang="ts">
import { computed, nextTick, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  api,
  type Claim,
  type LogLine,
  type LogQuery,
  type MaterializedObject,
  type Release,
  type WorkloadPod,
} from "../lib/api";
import { shortImage, timeAgo, uptime } from "../lib/format";
import { callerFor } from "../lib/me";
import { operatorMode } from "../lib/mode";
import { may } from "../lib/policy";
import { blockedPromotionFor } from "../lib/promotions";
import type { Tone } from "../lib/status";
import { useAsync, usePoll } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";
import CrashReport from "../components/CrashReport.vue";
import DomainsPanel from "../components/DomainsPanel.vue";
import FindingList from "../components/FindingList.vue";
import LogViewer from "../components/LogViewer.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
import RequestsPanel from "../components/RequestsPanel.vue";
import RequirementsPanel from "../components/RequirementsPanel.vue";
import ResourceHistory from "../components/ResourceHistory.vue";
import StatusDot from "../components/StatusDot.vue";

const route = useRoute();
const router = useRouter();
const toast = useToast();
const name = computed(() => route.params.name as string);

const { data, error, loading, refresh } = useAsync(async () => {
  const environment = await api.environment(name.value);
  // The project comes along for the role on it. An Environment carries no
  // role of its own — the API resolves which project a request is about from
  // the object it names — so the one place the caller's role is written down
  // is the project's own payload, and this screen's controls are keyed to it.
  const [releases, project, promotions, claims, exceptions] = await Promise.all([
    api.projectReleases(environment.project),
    api.project(environment.project),
    api.projectPromotions(environment.project, { environment: environment.name }),
    api.claims({ project: environment.project }),
    api.exceptions({ project: environment.project, environment: environment.name }),
  ]);
  return { environment, releases, project, promotions, claims, exceptions };
});
watch(name, () => void refresh());

const environment = computed(() => data.value?.environment);
// A promotion into this environment that stands blocked is the one thing the
// screen must not bury: the release everybody expects here is not here, and
// the unmet rules say why. Only the *newest* promotion counts — a blocked one
// a later promotion superseded is history.
const blockedPromotion = computed(() =>
  blockedPromotionFor(data.value?.promotions ?? [], data.value?.environment.name ?? ""),
);

// The active break-glass exceptions scoped to this environment. Loud on
// purpose and for as long as they stand: an environment running under a
// waiver must say so on its own screen, not only in the operator's register.
const activeExceptions = computed(() => data.value?.exceptions ?? []);

// The claims whose data derives from production — on a preview, the finding
// worth an alert rather than a table cell. The provenance shown is the
// provider's declaration for the claim; a preview's own branch carries the
// same declaration for the provider that ships (a Neon branch of production
// is production-derived).
const productionDerivedClaims = computed(() =>
  (data.value?.claims ?? []).filter((claim) => claim.dataProvenance === "production"),
);

function claimProvenanceFor(claim: Claim): string {
  return claim.dataProvenance ?? "";
}

// Redeploying and deleting are the project developer's; the materialized
// objects are the operator's and stay behind the mode toggle, which only an
// operator can now be on the far side of.
const caller = computed(() => callerFor(data.value?.project.role, data.value?.environment.project));
const mayDeploy = computed(() => may("PATCH /api/v1/environments/{name}", caller.value));
const mayDeleteEnvironment = computed(() => may("DELETE /api/v1/environments/{name}", caller.value));
const moving = computed(() => environment.value?.phase === "Deploying" || environment.value?.phase === "Pending");
usePoll(() => void refresh(), 5000, () => moving.value);

// What is running, fetched apart from the environment itself: a workload read
// that fails should cost its own panel, not the whole page.
const workload = useAsync(() => api.environmentWorkload(name.value));
watch(name, () => void workload.refresh());
usePoll(() => void workload.refresh(), 5000, () => moving.value);

// The diagnostics strip: what is wrong with this environment right now, from
// the same catalogue the operator's problems list reads, narrowed to this
// environment and its project. A saturated node or an unprogrammed Gateway is
// not on it — that belongs to the platform, and is on the operator's list.
//
// It is fetched apart from everything else and renders nothing when nothing is
// firing, so an environment that is fine looks exactly as it did before.
const signals = useAsync(() => api.environmentSignals(name.value));
watch(name, () => void signals.refresh());
usePoll(() => void signals.refresh(), 30_000, () => true);

// Findings link at a section of this page rather than at the top of it, so a
// `?section=` in the URL is scrolled to once there is something to scroll to.
const sectionIds: Record<string, string> = {
  requests: "section-requests",
  resources: "section-resources",
  workload: "section-workload",
};
watch(
  [() => route.query.section, data],
  async ([section, loaded]) => {
    if (!section || !loaded) return;
    const id = sectionIds[String(section)];
    if (!id) return;
    await nextTick();
    document.getElementById(id)?.scrollIntoView({ behavior: "smooth", block: "start" });
  },
  { immediate: true },
);

function podTone(pod: WorkloadPod): Tone {
  if (pod.ready) return "success";
  return pod.phase === "Failed" || pod.restarts > 0 ? "error" : "warning";
}

// The objects the operator materialized. Operator mode only — and fetched only
// once it is on, so the everyday view never pays for it.
const objects = useAsync(() => api.environmentObjects(name.value), { immediate: operatorMode.value });
watch([name, operatorMode], () => {
  if (operatorMode.value) void objects.refresh();
});

function manifestOf(object: MaterializedObject): string {
  return JSON.stringify(object.manifest ?? {}, null, 2);
}

async function copyManifest(object: MaterializedObject) {
  try {
    await navigator.clipboard.writeText(manifestOf(object));
    toast.add({ title: `${object.kind} copied`, color: "success", icon: "i-lucide-clipboard-check" });
  } catch (err) {
    toast.add({ title: "Copy failed", description: err instanceof Error ? err.message : String(err), color: "error" });
  }
}

const currentRelease = computed(() =>
  data.value?.releases.find((r) => r.name === environment.value?.release),
);
const otherReleases = computed(() =>
  (data.value?.releases ?? []).filter((r) => r.name !== environment.value?.release),
);

const target = ref<Release | null>(null);
const movingRelease = ref(false);
async function move() {
  if (!target.value || !environment.value) return;
  movingRelease.value = true;
  try {
    const outcome = await api.moveEnvironment(environment.value.name, target.value.name);
    // An environment with requirements answers with the promotion the move
    // became — nothing has moved yet, and the phase says what happens next.
    if ("trigger" in outcome) {
      toast.add({
        title: `Promotion ${outcome.name} requested`,
        description: `This environment declares requirements; the policy decides whether the release lands.`,
        color: "info",
        icon: "i-lucide-scale",
      });
    } else {
      toast.add({ title: `${environment.value.name} moved to ${target.value.name}`, color: "success", icon: "i-lucide-undo-2" });
    }
    target.value = null;
    await refresh();
  } catch (err) {
    toast.add({ title: "Move failed", description: err instanceof Error ? err.message : String(err), color: "error" });
  } finally {
    movingRelease.value = false;
  }
}

// Deleting is for previews only — a stuck one whose pull request the operator
// no longer tracks. Production is refused server-side; the button only shows
// for previews.
const confirmingDelete = ref(false);
const deleting = ref(false);
async function deleteEnvironment() {
  const env = environment.value;
  if (!env) return;
  deleting.value = true;
  try {
    await api.deleteEnvironment(env.name);
    toast.add({ title: `Preview ${env.name} is being torn down`, color: "success", icon: "i-lucide-trash-2" });
    void router.push({ name: "project", params: { name: env.project } });
  } catch (err) {
    toast.add({
      title: "Deleting the preview failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
    deleting.value = false;
    confirmingDelete.value = false;
  }
}

const logFetcher = (query: LogQuery) => api.environmentLogs(name.value, query);
const logStreamer = (query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
  api.streamEnvironmentLogs(name.value, query, onLine, signal);

// The history entry labels the release's fate; the "by" column says who moved
// the environment off it — a build for auto-promotions, a caller for API moves.
function historyLabel(reason: string): { label: string; tone: "neutral" | "warning" } {
  return reason === "rolledBack" ? { label: "Rolled back", tone: "warning" } : { label: "Superseded", tone: "neutral" };
}
function historyBy(entry: { reason: string; by?: string }): string {
  if (!entry.by) return "—";
  return entry.reason === "promoted" ? `build ${entry.by}` : entry.by;
}
</script>

<template>
  <div class="space-y-6">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <template v-else-if="environment">
      <!-- The newest promotion into this environment stands blocked: the
           release somebody expects here is not here, and these rules are
           why. -->
      <UAlert
        v-if="blockedPromotion"
        color="error"
        variant="soft"
        icon="i-lucide-shield-x"
        :title="`Promotion of ${blockedPromotion.release} is blocked`"
        :description="
          (blockedPromotion.unmetRules?.length ? `Unmet rules: ${blockedPromotion.unmetRules.join(', ')}. ` : '') +
          (blockedPromotion.message ?? '')
        "
      />
      <!-- A standing break-glass exception is shown as loudly as a blocked
           promotion, and for as long as it stands: this environment's bar is
           waived, on two people's word, until the stated moment. -->
      <UAlert
        v-for="exception in activeExceptions"
        :key="exception.name"
        color="warning"
        variant="soft"
        icon="i-lucide-alert-triangle"
        :title="`Break-glass exception ${exception.name} waives ${exception.ruleIDs.join(', ')} until ${new Date(exception.expiresAt).toLocaleString('en-GB')}`"
        :description="
          `${exception.reason} — requested by ${exception.requestedBy}, approved by ${exception.approvedBy}` +
          (exception.release ? `, scoped to ${exception.release}` : '') +
          (exception.incidentRef ? ` (${exception.incidentRef})` : '') +
          (exception.usedBy?.length ? `. Relied on by: ${exception.usedBy.join(', ')}` : '')
        "
      />
      <div class="flex items-start justify-between gap-4 flex-wrap">
        <div>
          <div class="flex items-center gap-2 text-xs text-muted mb-1">
            <RouterLink to="/" class="hover:text-highlighted">Overview</RouterLink>
            <span>/</span>
            <RouterLink :to="{ name: 'project', params: { name: environment.project } }" class="hover:text-highlighted">
              {{ environment.project }}
            </RouterLink>
            <span>/</span>
            <span class="text-toned font-mono">{{ environment.name }}</span>
          </div>
          <div class="flex items-center gap-3 flex-wrap">
            <h1 class="text-xl font-semibold text-highlighted">{{ environment.name }}</h1>
            <UBadge color="neutral" variant="subtle" size="sm">{{ environment.type }}</UBadge>
            <PhaseBadge :phase="environment.phase" />
            <!-- The rating is the owners' declaration; its absence is a state
                 of its own and is said out loud, never left blank. -->
            <UBadge v-if="environment.dataClass" color="warning" variant="subtle" size="sm" icon="i-lucide-shield">
              {{ environment.dataClass }}
            </UBadge>
            <UBadge v-else color="neutral" variant="outline" size="sm" icon="i-lucide-shield-question">
              unclassified
            </UBadge>
            <UBadge v-if="environment.residency" color="neutral" variant="subtle" size="sm" icon="i-lucide-globe">
              {{ environment.residency }}
            </UBadge>
          </div>
          <div class="flex items-center gap-3 mt-1 text-xs text-muted flex-wrap">
            <span v-if="environment.preview" class="font-mono">
              #{{ environment.preview.pullRequest }} · {{ environment.preview.branch }}
            </span>
            <span>created {{ timeAgo(environment.createdAt) }}</span>
          </div>
        </div>
        <div class="flex items-center gap-2">
          <UButton
            v-if="mayDeleteEnvironment && environment.type === 'preview'"
            color="neutral"
            variant="subtle"
            size="sm"
            icon="i-lucide-trash-2"
            @click="confirmingDelete = true"
          >
            Delete preview
          </UButton>
          <UButton
            v-if="environment.url"
            :href="environment.url"
            target="_blank"
            size="sm"
            icon="i-lucide-arrow-up-right"
            trailing
          >
            Open
          </UButton>
        </div>
      </div>

      <!-- What is wrong with it right now, if anything: the same findings the
           operator's problems list carries, narrowed to this environment. It
           renders nothing at all when nothing is firing and nothing went
           unread — and says so loudly when something went unread, because an
           empty strip is a claim that this environment is fine. -->
      <FindingList
        :answer="signals.data.value"
        :loading="signals.loading.value"
        :error="signals.error.value"
        variant="strip"
      />

      <!-- Preview deletion confirmation -->
      <UModal
        :open="confirmingDelete"
        :title="`Delete ${environment.name}?`"
        description="The preview's workload and route are torn down. A new build for its pull request recreates it."
        @update:open="(open: boolean) => { confirmingDelete = open; }"
      >
        <template #footer>
          <div class="flex justify-end gap-2 w-full">
            <UButton color="neutral" variant="subtle" @click="confirmingDelete = false">Cancel</UButton>
            <UButton color="error" :loading="deleting" icon="i-lucide-trash-2" @click="deleteEnvironment">
              Delete {{ environment.name }}
            </UButton>
          </div>
        </template>
      </UModal>

      <div class="rounded-md border border-default bg-muted px-5 py-4 grid gap-6 sm:grid-cols-3">
        <div>
          <p class="text-xs text-muted mb-1">Release</p>
          <p class="font-mono text-sm text-highlighted">{{ environment.release }}</p>
          <p class="text-xs text-dimmed mt-0.5">
            observed {{ environment.observedRelease || "—"
            }}<template v-if="environment.observedRelease && environment.observedRelease !== environment.release">
              — still rolling</template
            >
          </p>
        </div>
        <div class="min-w-0">
          <p class="text-xs text-muted mb-1">Image</p>
          <p class="font-mono text-sm text-toned truncate" :title="currentRelease?.image">
            {{ shortImage(currentRelease?.image) }}
          </p>
        </div>
        <div class="min-w-0">
          <p class="text-xs text-muted mb-1">URL</p>
          <a
            v-if="environment.url"
            :href="environment.url"
            target="_blank"
            rel="noopener"
            class="font-mono text-sm text-primary hover:underline break-all"
            >{{ environment.url }}</a
          >
          <p v-else class="text-sm text-dimmed">no route — see conditions</p>
        </div>
      </div>

      <!-- What the internet asked of it: the golden signals, the routes they
           were asked of, and the requests themselves. The ids are where a
           finding's `?section=` lands. -->
      <div id="section-requests">
        <RequestsPanel :environment="environment.name" :project="environment.project" :live="moving" />
      </div>

      <!-- A container that died, assembled: exit code, last lines, the memory
           that led there, the cluster's warnings, the edge's requests. One line
           when nothing has crashed, which is an answer rather than a shell. -->
      <CrashReport :environment="environment.name" :live="moving" />

      <div id="section-workload">
        <h2 class="text-sm font-medium text-highlighted mb-2">Workload</h2>
        <UAlert
          v-if="workload.error.value"
          color="error"
          variant="soft"
          icon="i-lucide-triangle-alert"
          :title="workload.error.value"
        />
        <div v-else-if="workload.data.value" class="rounded-md border border-default overflow-hidden">
          <p v-if="!workload.data.value.deployment" class="bg-muted px-5 py-4 text-sm text-muted">
            {{ workload.data.value.message || "Nothing is running for this environment yet." }}
          </p>
          <div v-else class="bg-muted px-5 py-4 grid gap-6 grid-cols-2 sm:grid-cols-4 text-sm">
            <div>
              <p class="text-xs text-muted mb-1">Replicas</p>
              <p class="font-mono text-highlighted">
                {{ workload.data.value.replicas.ready }} of {{ workload.data.value.replicas.desired }} ready
              </p>
            </div>
            <div>
              <p class="text-xs text-muted mb-1">Restarts</p>
              <p class="font-mono" :class="workload.data.value.restarts ? 'text-warning' : 'text-toned'">
                {{ workload.data.value.restarts }}
              </p>
            </div>
            <div>
              <p class="text-xs text-muted mb-1">Uptime</p>
              <p class="font-mono text-toned">{{ uptime(workload.data.value.startedAt) }}</p>
            </div>
            <div>
              <p class="text-xs text-muted mb-1">Resources</p>
              <p
                v-if="workload.data.value.resources"
                class="font-mono text-toned"
                :title="`limits ${workload.data.value.resources.cpuLimit || '—'} / ${workload.data.value.resources.memoryLimit || '—'}`"
              >
                {{ workload.data.value.resources.cpuRequest || "—" }} ·
                {{ workload.data.value.resources.memoryRequest || "—" }}
              </p>
              <p v-else class="text-dimmed">unset</p>
            </div>
          </div>

          <div v-if="workload.data.value.pods?.length" class="overflow-x-auto border-t border-default">
            <table class="w-full min-w-[42rem] text-sm">
              <thead>
                <tr class="text-left text-xs text-muted border-b border-default">
                  <th class="px-4 py-2 font-medium">Pod</th>
                  <th class="px-4 py-2 font-medium">Restarts</th>
                  <th class="px-4 py-2 font-medium">Node</th>
                  <th class="px-4 py-2 font-medium">Up</th>
                  <th class="px-4 py-2 font-medium">Detail</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="pod in workload.data.value.pods" :key="pod.name" class="border-b border-muted last:border-0">
                  <td class="px-4 py-2 font-mono text-highlighted">
                    <span class="inline-flex items-center gap-2">
                      <StatusDot :tone="podTone(pod)" />
                      <span class="truncate">{{ pod.name }}</span>
                    </span>
                  </td>
                  <td class="px-4 py-2 font-mono" :class="pod.restarts ? 'text-warning' : 'text-toned'">
                    {{ pod.restarts }}
                  </td>
                  <td class="px-4 py-2 font-mono text-xs text-toned">{{ pod.node || "—" }}</td>
                  <td class="px-4 py-2 text-xs text-muted whitespace-nowrap">{{ uptime(pod.startedAt) }}</td>
                  <td class="px-4 py-2 text-xs text-toned max-w-xs truncate" :title="pod.message || pod.phase">
                    {{ pod.message || pod.phase }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>

      <div id="section-resources">
        <ResourceHistory :environment="environment.name" :live="moving" />
      </div>

      <!-- The bar this environment sets, and how the deployed release
           measures up against it. Reading it is everybody's; the edit is
           the environment's owners' (or an operator's), which the panel
           decides from the same owners list the API enforces. -->
      <RequirementsPanel :environment="environment" :role="data?.project.role" @changed="refresh" />

      <!-- The data behind this environment: the project's claims, each with
           its class and what the provider says the data derives from. A
           preview running on production-derived data is the finding an
           auditor reaches first, so it is the one thing this section says
           loudly rather than politely. -->
      <div v-if="data?.claims.length" class="space-y-2">
        <h2 class="text-sm font-medium text-highlighted">Data</h2>
        <UAlert
          v-if="environment.type === 'preview' && productionDerivedClaims.length"
          color="warning"
          variant="soft"
          icon="i-lucide-database-zap"
          title="Production-derived data in this preview"
          :description="`${productionDerivedClaims.map((claim) => claim.name).join(', ')}: a branch of a production database is production-derived. The default policy refuses this where the environment declares requirements.`"
        />
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full text-xs">
            <tbody>
              <tr v-for="claim in data.claims" :key="claim.name" class="border-b border-default/50 last:border-0">
                <td class="px-3 py-2 font-mono text-highlighted">{{ claim.name }}</td>
                <td class="px-3 py-2 text-dimmed">{{ claim.type }}</td>
                <td class="px-3 py-2" :class="claim.dataClass ? 'text-toned' : 'text-dimmed'">
                  {{ claim.dataClass || "unclassified" }}
                </td>
                <td class="px-3 py-2">
                  <UBadge
                    v-if="claimProvenanceFor(claim) === 'production'"
                    :color="environment.type === 'preview' ? 'warning' : 'neutral'"
                    variant="subtle"
                    size="sm"
                  >
                    production-derived
                  </UBadge>
                  <span v-else-if="claimProvenanceFor(claim)" class="text-toned">
                    {{ claimProvenanceFor(claim) }}
                  </span>
                  <span v-else class="text-dimmed">undeclared</span>
                </td>
                <td class="px-3 py-2 text-dimmed">{{ claim.residency || "unknown location" }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <DomainsPanel :environment="environment.name" :role="data?.project.role" />

      <ConditionsTable v-if="operatorMode" :conditions="environment.conditions" />

      <div v-if="operatorMode">
        <h2 class="text-sm font-medium text-highlighted mb-2">Materialized objects</h2>
        <p class="text-xs text-muted mb-3">
          What the operator created in
          <span class="font-mono">{{ objects.data.value?.namespace || "the application namespace" }}</span> for this
          environment — the objects themselves, as the API server holds them.
        </p>
        <UAlert
          v-if="objects.error.value"
          color="error"
          variant="soft"
          icon="i-lucide-triangle-alert"
          :title="objects.error.value"
        />
        <div v-else class="rounded-md border border-default divide-y divide-default overflow-hidden">
          <details v-for="object in objects.data.value?.objects ?? []" :key="object.kind" class="group">
            <summary class="flex items-center gap-3 px-4 py-2.5 cursor-pointer text-sm hover:bg-elevated">
              <UIcon name="i-lucide-chevron-right" class="size-4 text-dimmed group-open:rotate-90" />
              <span class="font-mono text-highlighted">{{ object.kind }}</span>
              <span class="font-mono text-xs text-dimmed truncate">{{ object.name }}</span>
              <span v-if="!object.present" class="ml-auto text-xs text-warning">{{ object.message }}</span>
              <UButton
                v-else
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-copy"
                class="ml-auto"
                aria-label="Copy manifest"
                @click.prevent="copyManifest(object)"
              />
            </summary>
            <pre
              v-if="object.present"
              class="px-4 py-3 text-xs font-mono bg-muted overflow-x-auto max-h-96 overflow-y-auto"
              >{{ manifestOf(object) }}</pre
            >
          </details>
          <p v-if="!objects.data.value" class="px-4 py-3 text-xs text-muted">Loading…</p>
        </div>
      </div>

      <!-- The whole section is the move, not a list with a button on it: the
           releases themselves are already on the project's Deployments tab,
           so for a viewer this would be the same table twice. -->
      <div v-if="mayDeploy && otherReleases.length">
        <h2 class="text-sm font-medium text-highlighted mb-2">Move to another release</h2>
        <p class="text-xs text-muted mb-3">
          Rollback and promotion are the same one-field change: point the environment at an immutable release and the
          operator puts back exactly what it snapshotted.
        </p>
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[36rem] text-sm">
            <tbody>
              <tr v-for="release in otherReleases" :key="release.name" class="border-b border-muted last:border-0">
                <td class="px-4 py-2.5 font-mono text-highlighted w-44">{{ release.name }}</td>
                <td class="px-4 py-2.5 font-mono text-xs text-toned truncate max-w-xs" :title="release.image">
                  {{ shortImage(release.image) }}
                </td>
                <td class="px-4 py-2.5 text-xs text-muted whitespace-nowrap">{{ timeAgo(release.createdAt) }}</td>
                <td class="px-4 py-2.5 text-right">
                  <UButton color="neutral" variant="subtle" size="xs" @click="target = release">Move here</UButton>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div v-if="environment.history?.length">
        <h2 class="text-sm font-medium text-highlighted mb-2">Release history</h2>
        <p class="text-xs text-muted mb-3">
          How each release stopped being current — auto-promotions and moves through the dashboard alike, newest first.
        </p>
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[36rem] text-sm">
            <tbody>
              <tr v-for="entry in environment.history" :key="entry.release + entry.to" class="border-b border-muted last:border-0">
                <td class="px-4 py-2.5 font-mono text-highlighted w-44">{{ entry.release }}</td>
                <td class="px-4 py-2.5">
                  <UBadge :color="historyLabel(entry.reason).tone" variant="soft" size="sm">
                    {{ historyLabel(entry.reason).label }}
                  </UBadge>
                </td>
                <td class="px-4 py-2.5 text-xs text-toned truncate max-w-xs" :title="historyBy(entry)">
                  {{ historyBy(entry) }}
                </td>
                <td class="px-4 py-2.5 text-xs text-muted whitespace-nowrap">
                  current {{ timeAgo(entry.from) }} → {{ timeAgo(entry.to) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">Runtime logs</h2>
        <LogViewer :fetcher="logFetcher" :streamer="logStreamer" :live="moving" :query-clause="`environment = '${environment.name}'`" />
      </div>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>

    <UModal
      :open="target !== null"
      :title="`Move ${environment?.name}?`"
      :description="`The environment moves to ${target?.name}. Releases are immutable snapshots of image and config, so this is exact — and just as easy to undo.`"
      @update:open="(open: boolean) => { if (!open) target = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="target = null">Cancel</UButton>
          <UButton color="primary" :loading="movingRelease" icon="i-lucide-undo-2" @click="move">
            Move to {{ target?.name }}
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
