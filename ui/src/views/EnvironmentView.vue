<script setup lang="ts">
import { computed, nextTick, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  api,
  type Claim,
  type LogLine,
  type LogQuery,
  type Release,
} from "../lib/api";
import { exactTime, shortImage, shortSHA, timeAgo, uptime } from "../lib/format";
import { useFreshness } from "../lib/freshness";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { awaitingFirstDeployment } from "../lib/project";
import { blockedPromotionFor } from "../lib/promotions";
import { useAsync, usePoll } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";
import CrashReport from "../components/CrashReport.vue";
import DomainsPanel from "../components/DomainsPanel.vue";
import FindingList from "../components/FindingList.vue";
import LogViewer from "../components/LogViewer.vue";
import PageHeader from "../components/PageHeader.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
import ProcessesPanel from "../components/ProcessesPanel.vue";
import RequestsPanel from "../components/RequestsPanel.vue";
import RequirementsPanel from "../components/RequirementsPanel.vue";
import ResourceHistory from "../components/ResourceHistory.vue";
import RollbackPanel from "../components/RollbackPanel.vue";
import SourceLink from "../components/SourceLink.vue";

const route = useRoute();
const router = useRouter();
const toast = useToast();
// `:env` under a project, `:name` at the address this screen used to have.
const name = computed(() => (route.params.env ?? route.params.name) as string);

const { data, error, loading, refresh } = useAsync(async () => {
  const environment = await api.environment(name.value);
  // The project comes along for the role on it. An Environment carries no
  // role of its own — the API resolves which project a request is about from
  // the object it names — so the one place the caller's role is written down
  // is the project's own payload, and this screen's controls are keyed to it.
  // The builds come along for their commits. A release names the build it was
  // cut from and nothing else, so the rollback panel's "built from" and its
  // list of commits that stop being served are this join — done here, from a
  // list already served to a viewer, rather than by a route of its own.
  const [releases, builds, project, promotions, claims, exceptions] = await Promise.all([
    api.projectReleases(environment.project),
    api.projectBuilds(environment.project),
    api.project(environment.project),
    api.projectPromotions(environment.project, { environment: environment.name }),
    api.claims({ project: environment.project }),
    api.exceptions({ project: environment.project, environment: environment.name }),
  ]);
  return { environment, releases, builds, project, promotions, claims, exceptions };
});
watch(name, () => void refresh());

// An environment's address is its project's now, and `/environments/:name`
// cannot be redirected by the route table: only the environment knows which
// project it is in. So the old address opens this screen and the payload
// finishes the move. It is emitted by the API as a finding's evidence
// (`environmentEvidence`) with `?section=` naming the part of the page to open
// at, so the query travels with it — a redirect that dropped it would land the
// reader on the right screen showing the wrong thing.
watch(data, (loaded) => {
  if (!loaded || route.name !== "environment") return;
  void router.replace({
    name: "project-environment",
    params: { name: loaded.environment.project, env: loaded.environment.name },
    query: route.query,
    hash: route.hash,
  });
});

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
const mayRedeploy = computed(() => may("POST /api/v1/environments/{name}/redeploy", caller.value));
const moving = computed(() => environment.value?.phase === "Deploying" || environment.value?.phase === "Pending");
// Declared, and never deployed into (#491). Everything on this screen that
// asks about a running release has nothing to ask about, and the honest
// reading of that is a state rather than a page of dashes: what it demands is
// already in force, and the first build for it lands here.
const awaiting = computed(() => (environment.value ? awaitingFirstDeployment(environment.value) : false));
// How old this screen is, and the reader's hold on it: every fetch above
// reports into it and the header renders it.
const freshness = useFreshness();
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

// The pod table and the materialized objects that used to be here behind
// `<OperatorOnly>` are gone rather than ungated (#469). They are facts about
// the cluster rather than about this project, and the Platform scope is where
// facts about the cluster live now — Workloads for the pods, Events for what
// the API server said about them. What a developer asks of this screen is
// whether their environment is up, and that is the health strip, the crash
// report and the findings above.

const currentRelease = computed(() =>
  data.value?.releases.find((r) => r.name === environment.value?.release),
);
const otherReleases = computed(() =>
  (data.value?.releases ?? []).filter((r) => r.name !== environment.value?.release),
);

// Moving to another release is the rollback panel's, not a confirm dialog's
// (#181). The panel is three steps — pick, review the diff, verify — because
// this is the one destructive write the dashboard offers and the old dialog
// revealed nothing the release list had not already shown. The view keeps only
// which release it was opened on; everything else is the panel's own.
const rollbackOpen = ref(false);
const rollbackFrom = ref("");
function openRollback(release?: Release) {
  rollbackFrom.value = release?.name ?? "";
  rollbackOpen.value = true;
}
// `?rollback=1` opens it on arrival, which is what the overview's attention
// band links at: the reason for rolling back has already been read up there,
// and landing on the environment screen to hunt for the button again is the
// second navigation that band exists to remove. The panel is still the whole
// of the decision — this only opens it.
watch(
  [() => route.query.rollback, data],
  ([wanted, loaded]) => {
    if (wanted && loaded && mayDeploy.value) rollbackOpen.value = true;
  },
  { immediate: true },
);

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
    toast.add({
      title:
        env.type === "preview"
          ? `Preview ${env.name} is being torn down`
          : `Environment ${env.name} is being removed`,
      color: "success",
      icon: "i-lucide-trash-2",
    });
    void router.push({ name: "project", params: { name: env.project } });
  } catch (err) {
    toast.add({
      title: `Deleting ${env.name} failed`,
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
    deleting.value = false;
    confirmingDelete.value = false;
  }
}

// Redeploying is the same commit with today's settings (#392). It is beside
// rollback because it is the other half of one question — a release freezes
// the configuration it was cut with, so rollback is how you go back to an
// older one and this is how a corrected setting reaches the one you are on.
// The confirmation says what changes and what does not, because "redeploy" on
// its own reads like a synonym for "deploy" and is not one.
const confirmingRedeploy = ref(false);
const redeploying = ref(false);
async function redeployEnvironment() {
  const env = environment.value;
  if (!env) return;
  redeploying.value = true;
  try {
    const accepted = await api.redeployEnvironment(env.name);
    toast.add({
      title: accepted.promotion
        ? `${accepted.release} awaits ${env.name}'s requirements`
        : `${env.name} is deploying ${accepted.release}`,
      description: accepted.message,
      color: "success",
      icon: "i-lucide-refresh-cw",
    });
    confirmingRedeploy.value = false;
    await refresh();
  } catch (err) {
    toast.add({
      title: "Redeploying failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    redeploying.value = false;
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
// Where to go and look, for the reader who can: the pod carrying the refusal
// and the container of it the kubelet named. It is one line rather than a
// table because there is only ever one — the refusal is of the pod spec, so
// every replica carries the same one.
const refusalDetail = computed(() => {
  const refusal = environment.value?.refusal;
  if (!refusal) return "";
  // The container and the reason are facts about this environment; the pod
  // name is a fact about the cluster and is the Platform scope's (#469).
  return [refusal.container, refusal.reason].filter(Boolean).join(" · ");
});

function historyBy(entry: { reason: string; by?: string }): string {
  if (!entry.by) return "—";
  return entry.reason === "promoted" ? `build ${entry.by}` : entry.by;
}
</script>

<template>
  <div class="space-y-6">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <template v-else-if="environment">
      <!-- An environment declared before anything deployed into it. It is not
           a fault and is not drawn as one — the API says the same thing on the
           object, as Ready=False with reason AwaitingDeployment classified as
           information — and it is at the top because it explains every empty
           panel below it at once. -->
      <UAlert
        v-if="awaiting"
        color="neutral"
        variant="subtle"
        icon="i-lucide-hourglass"
        title="Nothing deployed here yet"
        description="This environment was declared before anything was deployed into it. What it demands is already in force: the first release to arrive is judged against it, and a build that lands here is what fills in everything below."
      />
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
      <!-- A container of this environment that the kubelet would not create.
           It is as loud as a blocked promotion and for the same reason:
           nothing here changes on its own, the environment is not going to
           start, and the sentence underneath is the whole diagnosis — it names
           the setting and the image. Before this it existed only as a
           container status nobody without cluster access could read (#393). -->
      <div v-if="environment.refusal" class="space-y-1">
        <UAlert
          color="error"
          variant="soft"
          icon="i-lucide-octagon-x"
          :title="`${environment.refusal.workload || 'This environment'} could not be started`"
          :description="environment.refusal.message"
        />
        <p v-if="refusalDetail" class="text-xs text-dimmed font-mono">
          {{ refusalDetail }}
        </p>
      </div>
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
      <PageHeader
        :freshness="freshness"
        :title="environment.name"
        :breadcrumb="[
          { label: 'Overview', to: '/' },
          { label: environment.project, to: { name: 'project', params: { name: environment.project } } },
          { label: environment.name, mono: true },
        ]"
      >
        <template #badges>
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
        </template>
        <template #meta>
          <!-- A preview exists because of a pull request, and the platform is
               the only thing that knows which one. -->
          <span v-if="environment.preview" class="font-mono">
            <SourceLink :href="environment.preview.pullRequestUrl">#{{ environment.preview.pullRequest }}</SourceLink> ·
            <SourceLink :href="environment.preview.branchUrl">{{ environment.preview.branch }}</SourceLink>
          </span>
          <span>created {{ timeAgo(environment.createdAt) }}</span>
        </template>
        <template #actions>
          <!-- Deleting one is defined by what is running in it: a preview,
               which a build recreates, and a declared environment nothing has
               deployed into, which holds a declaration and no software.
               Anything else is the project, and goes with it. -->
          <UButton
            v-if="mayDeleteEnvironment && (environment.type === 'preview' || awaiting)"
            color="neutral"
            variant="subtle"
            size="sm"
            icon="i-lucide-trash-2"
            @click="confirmingDelete = true"
          >
            {{ environment.type === "preview" ? "Delete preview" : "Delete environment" }}
          </UButton>
          <UButton
            v-if="mayRedeploy && environment.release"
            color="neutral"
            variant="subtle"
            size="sm"
            icon="i-lucide-refresh-cw"
            @click="confirmingRedeploy = true"
          >
            Redeploy
          </UButton>
          <UButton
            v-if="mayDeploy && otherReleases.length"
            color="neutral"
            variant="subtle"
            size="sm"
            icon="i-lucide-undo-2"
            @click="openRollback()"
          >
            Roll back
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
        </template>
      </PageHeader>

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

      <!-- Redeploy confirmation. It names both halves of the change, since
           the whole point of the action is that one of them does not move. -->
      <UModal
        :open="confirmingRedeploy"
        :title="`Redeploy ${environment.name}?`"
        :description="`The same commit as ${environment.release}, today's settings. A new release is cut from the commit this environment is already running, carrying the project's configuration as it stands now, and deployed here. The release running now is left as it is, so rolling back to it still puts back what was there.`"
        @update:open="(open: boolean) => { confirmingRedeploy = open; }"
      >
        <template #footer>
          <div class="flex justify-end gap-2 w-full">
            <UButton color="neutral" variant="subtle" @click="confirmingRedeploy = false">Cancel</UButton>
            <UButton :loading="redeploying" icon="i-lucide-refresh-cw" @click="redeployEnvironment">
              Redeploy {{ environment.name }}
            </UButton>
          </div>
        </template>
      </UModal>

      <!-- Preview deletion confirmation -->
      <UModal
        :open="confirmingDelete"
        :title="`Delete ${environment.name}?`"
        :description="
          environment.type === 'preview'
            ? 'The preview\'s workload and route are torn down. A new build for its pull request recreates it.'
            : 'Nothing is deployed here, so nothing is torn down — what goes is what this environment declares: its owners, the bar it sets, its classification and its tolerances. A later build for it creates it again, declaring none of them.'
        "
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
          <p v-if="awaiting" class="text-sm text-muted">nothing deployed yet</p>
          <p v-else class="font-mono text-sm text-highlighted">{{ environment.release }}</p>
          <p v-if="!awaiting" class="text-xs text-dimmed mt-0.5">
            observed {{ environment.observedRelease || "—"
            }}<template v-if="environment.observedRelease && environment.observedRelease !== environment.release">
              — still rolling</template
            >
          </p>
          <!-- The commit this is actually running, which is what a release
               name stands for and does not say. -->
          <p v-if="environment.git?.sha" class="text-xs text-dimmed font-mono mt-0.5">
            <SourceLink :href="environment.git.commitUrl">{{ shortSHA(environment.git.sha) }}</SourceLink> ·
            <SourceLink :href="environment.git.branchUrl">{{ environment.git.branch }}</SourceLink
            ><span v-if="environment.git.committedAt" :title="exactTime(environment.git.committedAt)">
              · committed {{ timeAgo(environment.git.committedAt) }}</span
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
          <!-- An internal project has no address by its own setting, which is
               not the same fact as a route that has not been programmed — and
               only the second one is worth reading the conditions over. -->
          <p v-else-if="environment.exposure === 'internal'" class="text-sm text-muted">
            internal — no hostname; reachable from the other applications here
          </p>
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

      <!-- What else this environment runs: its workers, its services and its
           scheduled jobs. Above the domains because a failing nightly job is a
           thing to trip over, and below the workload because the web process
           is still what most people came for — it is the one with the URL. -->
      <ProcessesPanel :environment="environment.name" :role="data?.project.role" />

      <DomainsPanel :environment="environment.name" :role="data?.project.role" />

      <!-- Everybody's. A developer whose environment is unhappy could not see
           the conditions saying why until #469 took the gate off. -->
      <ConditionsTable :conditions="environment.conditions" />

      <!-- The whole section is the move, not a list with a button on it: the
           releases themselves are already on the project's Deployments tab,
           so for a viewer this would be the same table twice.

           "Review" rather than "Move here", because that is now what the
           button does: the write is three steps away, behind a diff (#181). -->
      <div v-if="mayDeploy && otherReleases.length">
        <h2 class="text-sm font-medium text-highlighted mb-2">Move to another release</h2>
        <p class="text-xs text-muted mb-3">
          Rollback and promotion are the same one-field change: point the environment at an immutable release and the
          operator puts back exactly what it snapshotted. Reviewing one shows what that would change — the image, the
          variable snapshot, and the commits that stop being served — before anything is written.
        </p>
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[36rem] text-sm">
            <tbody>
              <tr v-for="release in otherReleases" :key="release.name" class="border-b border-muted last:border-0">
                <td class="px-3 py-2 font-mono text-highlighted w-44">{{ release.name }}</td>
                <td class="px-3 py-2 font-mono text-xs text-toned truncate max-w-xs" :title="release.image">
                  {{ shortImage(release.image) }}
                </td>
                <td class="px-3 py-2 text-xs text-muted whitespace-nowrap">{{ timeAgo(release.createdAt) }}</td>
                <td class="px-3 py-2 text-right">
                  <UButton color="neutral" variant="subtle" size="xs" @click="openRollback(release)">Review</UButton>
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
                <td class="px-3 py-2 font-mono text-highlighted w-44">{{ entry.release }}</td>
                <td class="px-3 py-2">
                  <UBadge :color="historyLabel(entry.reason).tone" variant="soft" size="sm">
                    {{ historyLabel(entry.reason).label }}
                  </UBadge>
                </td>
                <td class="px-3 py-2 text-xs text-toned truncate max-w-xs" :title="historyBy(entry)">
                  {{ historyBy(entry) }}
                </td>
                <td class="px-3 py-2 text-xs text-muted whitespace-nowrap">
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

    <!-- Pick, review the diff, verify. The panel outlives the write on
         purpose: closing on confirm would leave "did that work" to be
         answered by going and looking somewhere else. -->
    <RollbackPanel
      v-if="environment && data"
      :open="rollbackOpen"
      :environment="environment"
      :releases="data.releases"
      :builds="data.builds"
      :initial-release="rollbackFrom"
      @update:open="(open: boolean) => { rollbackOpen = open; }"
      @moved="refresh"
    />
  </div>
</template>
