<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api } from "../lib/api";
import { loadConfig, platformVersion, readVersion } from "../lib/config";
import { timeAgo } from "../lib/format";
import { inFlight, moving, stageOf, unreachable, versionLabel, type UpdateStage } from "../lib/updates";
import { useAsync, usePoll } from "../lib/useAsync";
import PhaseBadge from "./PhaseBadge.vue";
import UpdateFlight from "./UpdateFlight.vue";
import UpdateLogs from "./UpdateLogs.vue";

// The platform's own upgrades. Off unless the chart was installed with
// selfUpdate.enabled, which grants the update job cluster-admin — so when it is
// off the panel says how to turn it on rather than hiding.
//
// The panel watches an operation that takes away the API it is watching it
// through. Two reads follow it, and the second one exists because of the first:
//
//   - GET /updates is the record, and it stops answering half way through every
//     healthy upgrade, because applying the chart replaces the manager that
//     serves it. A failed read is therefore not an error while an update is
//     running — `unreachable` in lib/updates.ts is where that line is drawn.
//   - /config.json is public, static, served by the same operator and carries
//     its version. Polling it through the blackout is how this page learns the
//     upgrade landed without waiting for anything authenticated to come back,
//     and reading it updates the version the whole dashboard shows.
//
// A real failure of the upgrade still arrives the ordinary way: phase Failed,
// with helm's output underneath saying what it could not apply.

const toast = useToast();

// `recheck` decides which of the two reads the loader takes. The published
// versions are cached for an hour behind the API, so an installation that has
// just released something would otherwise have to wait it out or restart the
// operator the cache lives in.
const recheck = ref(false);

/** Whether the last read of /updates was answered at all — as opposed to
 *  answered with a refusal, which is an error like any other. */
const reachable = ref(true);

const updates = useAsync(async () => {
  try {
    const answer = await api.updates(recheck.value);
    reachable.value = true;
    return answer;
  } catch (err) {
    reachable.value = !unreachable(err);
    throw err;
  }
});

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

const items = computed(() => updates.data.value?.items ?? []);
const offered = computed(() => updates.data.value?.upgradableTo ?? []);
const target = ref<string>("");
watch(offered, (versions) => {
  if (!target.value || !versions.includes(target.value)) target.value = versions[0] ?? "";
});

// The version this page was loaded against, and the one the operator serving
// it reports now. They differ exactly once the upgrade has landed.
const startedOn = ref(platformVersion.value);
void loadConfig().then((config) => {
  if (!startedOn.value) startedOn.value = config.version;
});
const nowOn = computed(() => platformVersion.value);
const landedOn = ref("");
watch(platformVersion, (version) => {
  if (version && startedOn.value && version !== startedOn.value) landedOn.value = version;
});

// The update being followed. It is held by name rather than by value so that
// the record which comes back after the blackout — the same update, now
// Succeeded or Failed — replaces the one that went away, and the conclusion
// stays on screen instead of vanishing with the phase that put it there.
const followingName = ref("");
watch(
  () => inFlight(items.value),
  (update) => {
    if (update) followingName.value = update.name;
  },
  { immediate: true },
);
const flight = computed(() => items.value.find((update) => update.name === followingName.value) ?? null);
const following = computed(() => moving(flight.value));

const stage = computed<UpdateStage | null>(() =>
  flight.value
    ? stageOf({ phase: flight.value.phase, reachable: reachable.value, landed: !!landedOn.value })
    : null,
);

/** The blackout: an update in flight and an API that is not answering. It is
 *  the one case where a failed read is not reported as a failure. */
const blackout = computed(() => !reachable.value && following.value);

/** What is left of `updates.error` once the blackout has been accounted for.
 *  Everything else — a 403, a 500 with a body, the read failing when no
 *  upgrade is running — still renders exactly as it did. */
const readError = computed(() => (blackout.value ? null : updates.error.value));

// Both polls run only while an update is moving, and both are torn down with
// the panel: `usePoll` clears its timer on scope dispose.
usePoll(() => void updates.refresh(), 5000, () => following.value);
usePoll(() => void watchVersion(), 5000, () => following.value);

/** Ask the static config who is serving now. The first answer carrying a
 *  version other than the one this page started on is the new operator, which
 *  is also the moment the API is worth trying again. */
async function watchVersion() {
  try {
    const version = await readVersion();
    if (version !== startedOn.value && !reachable.value) void updates.refresh();
  } catch {
    // Nothing is serving the static files either. That is the middle of the
    // blackout, and the next tick asks again.
  }
}

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
    const started = await api.startUpdate(target.value);
    // A second upgrade in one session starts from where this one left off:
    // whatever is serving now is the version the next landing is measured
    // against.
    followingName.value = started.name;
    landedOn.value = "";
    startedOn.value = platformVersion.value || startedOn.value;
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

// helm's output for a finished upgrade, one row at a time. The job is reaped
// an hour after it ends and the lines outlive it, so the history is readable
// long after the thing that wrote it is gone.
const opened = ref("");
function toggleOutput(name: string) {
  opened.value = opened.value === name ? "" : name;
}
</script>

<template>
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
          :disabled="following"
          title="Ask the registry again instead of the hour-long cache"
          @click="recheckVersions"
        >
          Check for updates
        </UButton>
        <p class="text-xs text-muted font-mono">running {{ versionLabel(nowOn) }}</p>
      </div>
    </div>

    <UAlert v-if="readError" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="readError" />

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
        <!-- The upgrade in flight, and it stays after it finishes: the record
             that says how it ended arrives with the operator that survived
             it. -->
        <UpdateFlight
          v-if="flight && stage"
          :update="flight"
          :stage="stage"
          :started-on="startedOn"
          :now-on="nowOn"
        />

        <UAlert
          v-else-if="updates.data.value.discoveryError"
          color="warning"
          variant="soft"
          icon="i-lucide-cloud-off"
          title="The published versions could not be listed"
          :description="updates.data.value.discoveryError"
        />

        <template v-if="!following">
          <template v-if="offered.length">
            <div class="flex flex-wrap items-end gap-3">
              <UFormField label="Upgrade to" help="Applies the chart at this version and waits for it to come up.">
                <USelect v-model="target" :items="offered" class="w-40 font-mono" />
              </UFormField>
              <UButton :loading="upgrading" :disabled="!target" icon="i-lucide-arrow-up-circle" @click="startUpdate">
                Update platform
              </UButton>
            </div>
            <p v-if="heldBack" class="text-xs text-muted">
              {{ heldBack }} has been published, but it crosses a minor version — pre-1.0 that is where breaking changes
              land, and its release notes may name manual steps. Set
              <span class="font-mono">selfUpdate.allowMinor=true</span> to offer these here.
            </p>
          </template>
          <p v-else-if="!updates.data.value.discoveryError" class="text-sm text-muted">
            The platform is on the newest version it can move to.
            <template v-if="heldBack">
              {{ heldBack }} is available but crosses a minor version; set
              <span class="font-mono">selfUpdate.allowMinor=true</span> to offer it.
            </template>
          </p>
        </template>
      </template>

      <div v-if="items.length" class="rounded-md border border-default bg-muted overflow-x-auto">
        <table class="w-full min-w-[36rem] text-sm">
          <thead>
            <tr class="text-left text-xs text-muted border-b border-default">
              <th class="px-3 py-2 font-medium">Version</th>
              <th class="px-3 py-2 font-medium">Phase</th>
              <th class="px-3 py-2 font-medium">Requested by</th>
              <th class="px-3 py-2 font-medium">Message</th>
              <th class="px-3 py-2 font-medium text-right">Output</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="update in items.slice(0, 5)" :key="update.name">
              <tr class="border-b border-muted last:border-0">
                <td class="px-3 py-2 font-mono text-highlighted">
                  <template v-if="update.fromVersion">{{ update.fromVersion }} → </template>{{ update.version }}
                </td>
                <td class="px-3 py-2"><PhaseBadge :phase="update.phase" /></td>
                <td class="px-3 py-2 text-xs text-toned">{{ update.requestedBy || "—" }}</td>
                <td class="px-3 py-2 text-xs text-toned max-w-md truncate" :title="update.message">
                  {{ update.message || "—" }}
                </td>
                <td class="px-3 py-2 text-right">
                  <button class="text-xs text-primary hover:underline" @click="toggleOutput(update.name)">
                    {{ opened === update.name ? "hide" : "helm output" }}
                  </button>
                </td>
              </tr>
              <tr v-if="opened === update.name" class="border-b border-muted last:border-0">
                <td colspan="5" class="px-3 py-3">
                  <!-- The one being followed above already has a tail on it;
                       a second one here would be two streams of one log. -->
                  <UpdateLogs :name="update.name" :live="moving(update) && flight?.name !== update.name" />
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>
    </template>
  </div>
</template>
