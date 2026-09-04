<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api } from "../lib/api";
import { loadConfig, platformVersion } from "../lib/config";
import { versionLabel } from "../lib/updates";
import { useAsync } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";
import NotificationsPanel from "../components/NotificationsPanel.vue";
import OperatorsPanel from "../components/OperatorsPanel.vue";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";
import PlatformUpdatePanel from "../components/PlatformUpdatePanel.vue";
import RetentionPanel from "../components/RetentionPanel.vue";
import StatusDot from "../components/StatusDot.vue";

// The Kitchen singleton: platform-wide configuration, editable from here —
// which is the reason it is a custom resource and not just Helm values.

const toast = useToast();
const { data: settings, error, loading, refresh } = useAsync(() => api.settings());

// The platform as it is running, next to the platform as it is configured.
// The component survey is the part that catches what conditions cannot: a
// workload whose pods were refused at admission has no pods to look at.
const status = useAsync(() => api.status());
const components = computed(() => status.data.value?.components ?? []);

// The release, from /config.json rather than the settings API: it is a fact
// about the running operator, not part of the singleton anyone can edit here.
// It is read through `platformVersion` rather than off the loaded config,
// because a platform upgrade replaces the operator serving this page and the
// update panel below re-reads the number when it does.
void loadConfig();
const version = computed(() => versionLabel(platformVersion.value));

const strategy = ref<string>("auto");
const concurrency = ref<number>(2);
// The ceiling one build runs under, kept as the quantity strings the API
// carries ("2", "500m", "4Gi"). An empty one is the installation that has
// decided its builds are unbounded, not a box nobody has filled in yet.
const buildCPU = ref<string>("");
const buildMemory = ref<string>("");
// How long a build may run before the platform ends it. 0 is a setting — no
// deadline at all — and not an empty box.
const buildTimeout = ref<number>(60);
const releaseRetention = ref<number>(10);
// How many preview environments one project may have live at once. 0 is a
// setting — no ceiling at all — and not an empty box, the same way the build
// timeout's 0 is.
const previewsMax = ref<number>(5);
const retention = ref<number>(30);

watch(settings, (value) => {
  if (!value) return;
  strategy.value = value.buildStrategy || "auto";
  concurrency.value = value.buildConcurrency ?? 2;
  buildCPU.value = value.buildCPU ?? "";
  buildMemory.value = value.buildMemory ?? "";
  buildTimeout.value = value.buildTimeoutMinutes ?? 60;
  releaseRetention.value = value.releaseRetention ?? 10;
  previewsMax.value = value.previewsMaxPerProject ?? 5;
  retention.value = value.logRetentionDays ?? 30;
});

const dirty = computed(() => {
  const s = settings.value;
  if (!s) return false;
  return (
    strategy.value !== (s.buildStrategy || "auto") ||
    concurrency.value !== (s.buildConcurrency ?? 2) ||
    buildCPU.value !== (s.buildCPU ?? "") ||
    buildMemory.value !== (s.buildMemory ?? "") ||
    buildTimeout.value !== (s.buildTimeoutMinutes ?? 60) ||
    releaseRetention.value !== (s.releaseRetention ?? 10) ||
    previewsMax.value !== (s.previewsMaxPerProject ?? 5) ||
    retention.value !== (s.logRetentionDays ?? 30)
  );
});

/** What the platform's builds can take from the cluster: the ceiling times the
 *  concurrency. It is the sentence the two settings only mean together, which
 *  is why it is on the screen rather than left to be worked out. */
/** The preview ceiling in the same voice as the build footprint: the two are
 *  the platform's two statements about what a push may take, and this one has
 *  no arithmetic to show, only what happens at the ceiling. */
const previewCeiling = computed(() => {
  if (previewsMax.value <= 0) {
    return "No ceiling: a project has as many previews as it has open pull requests, and each one costs a copy of every backing service it claims.";
  }
  return `At most ${previewsMax.value} live previews per project. A pull request past it gets a commit status and a comment instead of an environment, and its preview on the next push after a slot frees.`;
});

const buildFootprint = computed(() => {
  const ceiling = [buildCPU.value.trim(), buildMemory.value.trim()].filter(Boolean).join(" + ");
  if (!ceiling) {
    return "No ceiling: builds reserve nothing, and one of them can take the node from what is already running.";
  }
  return `At most ${concurrency.value} × ${ceiling} — reserved for builds while they run, and the most they can take.`;
});

const saving = ref(false);
async function save() {
  saving.value = true;
  try {
    await api.updateSettings({
      buildStrategy: strategy.value,
      buildConcurrency: concurrency.value,
      buildCPU: buildCPU.value.trim(),
      buildMemory: buildMemory.value.trim(),
      buildTimeoutMinutes: buildTimeout.value,
      releaseRetention: releaseRetention.value,
      previewsMaxPerProject: previewsMax.value,
      logRetentionDays: retention.value,
    });
    toast.add({ title: "Settings saved", color: "success", icon: "i-lucide-check" });
    await refresh();
  } catch (err) {
    toast.add({ title: "Saving failed", description: err instanceof Error ? err.message : String(err), color: "error" });
  } finally {
    saving.value = false;
  }
}

const strategies = [
  { label: "auto — detect the framework", value: "auto" },
  { label: "dockerfile — the project's own", value: "dockerfile" },
  { label: "buildpacks", value: "buildpacks" },
];
</script>

<template>
  <div class="space-y-6">
    <PageHeader title="Settings" :breadcrumb="[{ label: 'Platform', to: '/platform' }, { label: 'Settings' }]">
      <template #description>
        The platform's runtime configuration — the <span class="font-mono">Kitchen</span> singleton the operator
        reconciles.
      </template>
    </PageHeader>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <template v-else-if="settings">
      <div
        class="rounded-md border border-default bg-muted px-5 py-4 grid gap-x-8 gap-y-4 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-5 text-sm"
      >
        <div>
          <p class="text-xs text-muted mb-0.5">Base domain</p>
          <p class="font-mono text-toned">{{ settings.baseDomain || "—" }}</p>
        </div>
        <div class="min-w-0">
          <p class="text-xs text-muted mb-0.5">API</p>
          <p class="font-mono text-toned truncate" :title="settings.apiExternalURL">
            {{ settings.apiExternalURL || "—" }}
          </p>
        </div>
        <div>
          <p class="text-xs text-muted mb-0.5">Identity provider</p>
          <p class="font-mono text-toned">
            {{ settings.authEnabled ? settings.authHost || "enabled" : "disabled" }}
          </p>
        </div>
        <div>
          <p class="text-xs text-muted mb-0.5">Gateway</p>
          <p class="font-mono text-toned">
            {{ settings.gatewayClassName || "—" }}<template v-if="settings.gatewayAddress">
              · {{ settings.gatewayAddress }}</template
            >
          </p>
          <!-- The Gateway's address is where traffic lands inside the cluster,
               which is not where it arrives from the internet when a router
               forwards to it. Both belong on the screen an operator reads when
               nothing is reachable. -->
          <p v-if="settings.publicAddresses?.length" class="text-[11px] text-muted mt-0.5">
            reached at {{ settings.publicAddresses.join(", ") }}
          </p>
        </div>
        <div>
          <p class="text-xs text-muted mb-0.5">Version</p>
          <p class="font-mono text-toned">{{ version }}</p>
        </div>
      </div>

      <!-- Two columns once the viewport is wide enough for the widest table
           inside one of them; a single column below that, in the order the
           panels are written. -->
      <div class="grid gap-6 items-start 2xl:grid-cols-2">
        <div class="space-y-6">
          <OperatorsPanel :settings="settings" @saved="refresh" />

          <div class="rounded-md border border-default px-5 py-4 space-y-4">
            <h2 class="text-sm font-medium text-highlighted">Builds, previews and telemetry</h2>
            <div class="grid gap-4 sm:grid-cols-2">
              <UFormField label="Default build strategy" help="Projects can override this per repository.">
                <USelect v-model="strategy" :items="strategies" class="w-full" />
              </UFormField>
              <UFormField label="Build concurrency" help="How many builds run at once, platform-wide.">
                <UInputNumber v-model="concurrency" :min="1" :max="32" class="w-40" />
              </UFormField>
              <!-- The ceiling sits beside the concurrency because neither bounds
                   anything alone: what the platform's builds can take is one
                   times the other, which the line under the fields says. -->
              <UFormField
                label="CPU per build"
                help="A Kubernetes quantity — 2, 500m. Reserved for the build and capped at it. Empty for no ceiling."
              >
                <UInput v-model="buildCPU" placeholder="2" class="w-40" />
              </UFormField>
              <UFormField
                label="Memory per build"
                help="A Kubernetes quantity — 4Gi, 512Mi. A build that reaches it is killed and fails saying so. Empty for no ceiling."
              >
                <UInput v-model="buildMemory" placeholder="4Gi" class="w-40" />
              </UFormField>
              <!-- The ceiling in time, beside the ceiling in capacity. It is
                   the Job's own deadline, which a Job cannot be told to change
                   once it exists — so the help says which builds a change
                   reaches rather than leaving it to be found out. -->
              <UFormField
                label="Build timeout (minutes)"
                help="How long one build may run before the platform ends it and the commit reports a failed build. 0 means no deadline. Applies to builds started after the change."
              >
                <UInputNumber v-model="buildTimeout" :min="0" :max="1440" class="w-40" />
              </UFormField>
              <UFormField
                label="Releases kept per project"
                help="Older releases are pruned. One an environment still runs is always kept, so a rollback target never disappears. 0 keeps every release."
              >
                <UInputNumber v-model="releaseRetention" :min="0" :max="500" class="w-40" />
              </UFormField>
              <!-- The preview ceiling sits with the build ceiling because
                   the two are the same statement about different things: how
                   much of this cluster a project may take by pushing. -->
              <UFormField
                label="Preview environments per project"
                help="How many previews one project may have live at once. A pull request past the ceiling is told so on the request rather than started. 0 means no ceiling."
              >
                <UInputNumber v-model="previewsMax" :min="0" :max="100" class="w-40" />
              </UFormField>
              <UFormField
                label="Default telemetry retention (days)"
                help="The number every telemetry class inherits. Setting a class in the retention panel overrides it for that class."
              >
                <UInputNumber v-model="retention" :min="1" :max="365" class="w-40" />
              </UFormField>
            </div>
            <p class="text-xs text-muted">{{ buildFootprint }}</p>
            <p class="text-xs text-muted">{{ previewCeiling }}</p>
            <div class="flex justify-end">
              <UButton :disabled="!dirty" :loading="saving" icon="i-lucide-save" @click="save">Save changes</UButton>
            </div>
          </div>
        </div>

        <div class="space-y-6">
          <RetentionPanel />

          <!-- Every address this installation posts its activity to,
               project-scoped ones included: "who is being told what" is one
               question, and this is the screen it is asked on. -->
          <NotificationsPanel />

          <PlatformUpdatePanel />
        </div>
      </div>

      <!-- Both are five-column tables with a message that truncates, so they
           keep the whole width rather than sharing it.

           Neither is gated on the mode, and that is the rule rather than an
           oversight: a platform screen is the operator's screen entire (see
           docs/UI.md). Gating blocks *inside* one is how this page came to
           show its top half and not its bottom to an operator who had chosen
           the developer's view and then followed a link here. -->
      <PageSection title="Platform conditions">
        <ConditionsTable :conditions="settings.conditions" />
      </PageSection>

      <PageSection
        title="Platform components"
        description="Every workload labelled as part of Kitchen, and whether the pods it wants are running. A component with no pods at all was refused before it started — the message carries the reason."
      >
        <div class="rounded-md border border-default bg-muted overflow-x-auto">
          <table class="w-full min-w-[36rem] text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default">
                <th class="px-3 py-2 font-medium">Component</th>
                <th class="px-3 py-2 font-medium">Kind</th>
                <th class="px-3 py-2 font-medium">Pods</th>
                <th class="px-3 py-2 font-medium">Message</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!components.length">
                <td colspan="4" class="px-3 py-2 text-muted">No components surveyed yet.</td>
              </tr>
              <tr v-for="component in components" :key="component.name" class="border-b border-muted last:border-0">
                <td class="px-3 py-2 font-mono text-highlighted">
                  <span class="inline-flex items-center gap-2">
                    <StatusDot :tone="component.healthy ? 'success' : 'error'" />
                    {{ component.name }}
                  </span>
                </td>
                <td class="px-3 py-2 font-mono text-xs text-toned">{{ component.kind }}</td>
                <td
                  class="px-3 py-2 font-mono whitespace-nowrap"
                  :class="component.healthy ? 'text-toned' : 'text-error'"
                >
                  {{ component.available }} / {{ component.desired }}
                </td>
                <td class="px-3 py-2 text-xs text-toned max-w-md truncate" :title="component.message">
                  {{ component.message || "—" }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </PageSection>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>
  </div>
</template>
