<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api } from "../lib/api";
import { loadConfig } from "../lib/config";
import { operatorMode } from "../lib/mode";
import { timeAgo } from "../lib/format";
import { useAsync, usePoll } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
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
const platform = useAsync(() => loadConfig());
const version = computed(() => {
  const v = platform.data.value?.version;
  if (!v) return "—";
  return v === "dev" ? "dev" : `v${v}`;
});

const strategy = ref<string>("auto");
const concurrency = ref<number>(2);
const releaseRetention = ref<number>(10);
const retention = ref<number>(30);

watch(settings, (value) => {
  if (!value) return;
  strategy.value = value.buildStrategy || "auto";
  concurrency.value = value.buildConcurrency ?? 2;
  releaseRetention.value = value.releaseRetention ?? 10;
  retention.value = value.logRetentionDays ?? 30;
});

const dirty = computed(() => {
  const s = settings.value;
  if (!s) return false;
  return (
    strategy.value !== (s.buildStrategy || "auto") ||
    concurrency.value !== (s.buildConcurrency ?? 2) ||
    releaseRetention.value !== (s.releaseRetention ?? 10) ||
    retention.value !== (s.logRetentionDays ?? 30)
  );
});

const saving = ref(false);
async function save() {
  saving.value = true;
  try {
    await api.updateSettings({
      buildStrategy: strategy.value,
      buildConcurrency: concurrency.value,
      releaseRetention: releaseRetention.value,
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

// The platform's own upgrades. Off unless the chart was installed with
// selfUpdate.enabled, which grants the update job cluster-admin — so when it
// is off the panel says how to turn it on rather than hiding.
// `recheck` decides which of the two reads the loader takes. The published
// versions are cached for an hour behind the API, so an installation that has
// just released something would otherwise have to wait it out or restart the
// operator the cache lives in.
const recheck = ref(false);
const updates = useAsync(() => api.updates(recheck.value));
const rechecking = ref(false);
async function recheckVersions() {
  rechecking.value = true;
  recheck.value = true;
  try {
    await updates.refresh();
  } finally {
    recheck.value = false;
    rechecking.value = false;
  }
}
const offered = computed(() => updates.data.value?.upgradableTo ?? []);
const target = ref<string>("");
watch(offered, (versions) => {
  if (!target.value || !versions.includes(target.value)) target.value = versions[0] ?? "";
});

const inFlight = computed(() =>
  (updates.data.value?.items ?? []).find((u) => u.phase === "Running" || u.phase === "Pending"),
);
// The upgrade replaces the operator serving this page, so the poll is also
// how the dashboard notices it has come back on a new version.
usePoll(() => void updates.refresh(), 5000, () => !!inFlight.value);

// A release newer than anything on offer is a minor crossing held back by
// selfUpdate.allowMinor — worth naming, since it is the upgrade whose notes
// may carry manual steps.
const heldBack = computed(() => {
  const u = updates.data.value;
  if (!u?.latestVersion || u.allowMinor) return "";
  return offered.value.includes(u.latestVersion) ? "" : u.latestVersion;
});

const upgrading = ref(false);
async function startUpdate() {
  if (!target.value) return;
  upgrading.value = true;
  try {
    await api.startUpdate(target.value);
    toast.add({
      title: `Upgrading to ${target.value}`,
      description: "The operator restarts part-way through; this page will follow it.",
      color: "success",
      icon: "i-lucide-arrow-up-circle",
    });
    await updates.refresh();
  } catch (err) {
    toast.add({
      title: "The upgrade was not started",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    upgrading.value = false;
  }
}
</script>

<template>
  <div class="space-y-6 max-w-3xl">
    <div>
      <h1 class="text-xl font-semibold text-highlighted">Settings</h1>
      <p class="text-xs text-muted mt-1">
        The platform's runtime configuration — the <span class="font-mono">Kitchen</span> singleton the operator
        reconciles.
      </p>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <template v-else-if="settings">
      <div class="rounded-md border border-default bg-muted px-5 py-4 grid gap-x-8 gap-y-4 sm:grid-cols-2 text-sm">
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
        </div>
        <div>
          <p class="text-xs text-muted mb-0.5">Version</p>
          <p class="font-mono text-toned">{{ version }}</p>
        </div>
      </div>

      <div class="rounded-md border border-default px-5 py-4 space-y-4">
        <h2 class="text-sm font-medium text-highlighted">Builds and telemetry</h2>
        <UFormField label="Default build strategy" help="Projects can override this per repository.">
          <USelect v-model="strategy" :items="strategies" class="w-full max-w-72" />
        </UFormField>
        <UFormField label="Build concurrency" help="How many builds run at once, platform-wide.">
          <UInputNumber v-model="concurrency" :min="1" :max="32" class="w-40" />
        </UFormField>
        <UFormField
          label="Releases kept per project"
          help="Older releases are pruned. One an environment still runs is always kept, so a rollback target never disappears. 0 keeps every release."
        >
          <UInputNumber v-model="releaseRetention" :min="0" :max="500" class="w-40" />
        </UFormField>
        <UFormField label="Log retention (days)" help="TTL the operator keeps on the ClickHouse telemetry tables.">
          <UInputNumber v-model="retention" :min="1" :max="365" class="w-40" />
        </UFormField>
        <div class="flex justify-end">
          <UButton :disabled="!dirty" :loading="saving" icon="i-lucide-save" @click="save">Save changes</UButton>
        </div>
      </div>

      <div class="rounded-md border border-default px-5 py-4 space-y-4">
        <div class="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-2">
          <h2 class="text-sm font-medium text-highlighted">Platform updates</h2>
          <div class="flex items-center gap-3">
            <p v-if="updates.data.value?.checkedAt" class="text-xs text-muted">
              checked {{ timeAgo(updates.data.value.checkedAt) }}
            </p>
            <UButton
              v-if="updates.data.value?.enabled"
              size="xs"
              color="neutral"
              variant="ghost"
              icon="i-lucide-refresh-cw"
              :loading="rechecking"
              :disabled="!!inFlight"
              title="Ask the registry again instead of the hour-long cache"
              @click="recheckVersions"
            >
              Check for updates
            </UButton>
            <p class="text-xs text-muted font-mono">running {{ version }}</p>
          </div>
        </div>

        <UAlert
          v-if="updates.error.value"
          color="error"
          variant="soft"
          icon="i-lucide-triangle-alert"
          :title="updates.error.value"
        />

        <template v-else-if="updates.data.value">
          <UAlert
            v-if="!updates.data.value.enabled"
            color="neutral"
            variant="soft"
            icon="i-lucide-lock"
            title="This installation does not update itself"
            :description="updates.data.value.reason"
          />

          <template v-else>
            <UAlert
              v-if="inFlight"
              color="info"
              variant="soft"
              icon="i-lucide-loader"
              :title="`Upgrading to ${inFlight.version}`"
              :description="inFlight.message || 'The operator is being replaced by the version it is installing.'"
            />
            <UAlert
              v-else-if="updates.data.value.discoveryError"
              color="warning"
              variant="soft"
              icon="i-lucide-cloud-off"
              title="The published versions could not be listed"
              :description="updates.data.value.discoveryError"
            />
            <template v-else-if="offered.length">
              <div class="flex flex-wrap items-end gap-3">
                <UFormField label="Upgrade to" help="Applies the chart at this version and waits for it to come up.">
                  <USelect v-model="target" :items="offered" class="w-40 font-mono" />
                </UFormField>
                <UButton
                  :loading="upgrading"
                  :disabled="!target"
                  icon="i-lucide-arrow-up-circle"
                  @click="startUpdate"
                >
                  Update platform
                </UButton>
              </div>
              <p v-if="heldBack" class="text-xs text-muted">
                {{ heldBack }} has been published, but it crosses a minor version — pre-1.0 that is where breaking
                changes land, and its release notes may name manual steps. Set
                <span class="font-mono">selfUpdate.allowMinor=true</span> to offer these here.
              </p>
            </template>
            <p v-else class="text-sm text-muted">
              The platform is on the newest version it can move to.
              <template v-if="heldBack">
                {{ heldBack }} is available but crosses a minor version; set
                <span class="font-mono">selfUpdate.allowMinor=true</span> to offer it.
              </template>
            </p>
          </template>

          <div v-if="updates.data.value.items.length" class="rounded-md border border-default bg-muted overflow-x-auto">
            <table class="w-full min-w-[36rem] text-sm">
              <thead>
                <tr class="text-left text-xs text-muted border-b border-default">
                  <th class="px-3 py-2 font-medium">Version</th>
                  <th class="px-3 py-2 font-medium">Phase</th>
                  <th class="px-3 py-2 font-medium">Requested by</th>
                  <th class="px-3 py-2 font-medium">Message</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="update in updates.data.value.items.slice(0, 5)"
                  :key="update.name"
                  class="border-b border-muted last:border-0"
                >
                  <td class="px-3 py-2 font-mono text-highlighted">
                    <template v-if="update.fromVersion">{{ update.fromVersion }} → </template>{{ update.version }}
                  </td>
                  <td class="px-3 py-2"><PhaseBadge :phase="update.phase" /></td>
                  <td class="px-3 py-2 text-xs text-toned">{{ update.requestedBy || "—" }}</td>
                  <td class="px-3 py-2 text-xs text-toned max-w-md truncate" :title="update.message">
                    {{ update.message || "—" }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </template>
      </div>

      <div v-if="operatorMode">
        <h2 class="text-sm font-medium text-highlighted mb-2">Platform conditions</h2>
        <ConditionsTable :conditions="settings.conditions" />
      </div>

      <div v-if="operatorMode">
        <h2 class="text-sm font-medium text-highlighted mb-2">Platform components</h2>
        <p class="text-xs text-muted mb-3">
          Every workload labelled as part of Kitchen, and whether the pods it wants are running. A component with no
          pods at all was refused before it started — the message carries the reason.
        </p>
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
                <td colspan="4" class="px-3 py-3 text-muted">No components surveyed yet.</td>
              </tr>
              <tr v-for="component in components" :key="component.name" class="border-b border-muted last:border-0">
                <td class="px-3 py-2 font-mono text-highlighted">
                  <span class="inline-flex items-center gap-2">
                    <StatusDot :tone="component.healthy ? 'success' : 'error'" />
                    {{ component.name }}
                  </span>
                </td>
                <td class="px-3 py-2 font-mono text-xs text-toned">{{ component.kind }}</td>
                <td class="px-3 py-2 font-mono" :class="component.healthy ? 'text-toned' : 'text-error'">
                  {{ component.available }} / {{ component.desired }}
                </td>
                <td class="px-3 py-2 text-xs text-toned max-w-md truncate" :title="component.message">
                  {{ component.message || "—" }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>
  </div>
</template>
