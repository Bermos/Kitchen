<script setup lang="ts">
import { computed, ref } from "vue";
import { api, APIError, type Addon, type AddonUpgrade, type AddonUpgrades } from "../lib/api";
import { exactTime, timeAgo } from "../lib/format";
import { conditionSeverity } from "../lib/status";
import { useAsync } from "../lib/useAsync";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";
import StatusDot from "../components/StatusDot.vue";

// The platform's own dependencies: what this operator can install into the
// cluster it owns, what is actually there, and who put it there.
//
// This is a platform screen, so it is the operator's entire and gates nothing
// inside itself. It is also the one screen where operator vocabulary is the
// subject rather than an aside — helm releases, namespaces, cluster-admin —
// because the question it answers is "may this cluster do X, and what would it
// cost to let it".
//
// Three things it deliberately shows for an entry nobody has installed. The
// catalogue is compiled into the operator, so the list is the same everywhere
// and an entry that is *not* permitted is still a row: knowing what the
// platform could run, and the single value that would let it, is why somebody
// opened this page. And the grant is spelled out before it is made rather than
// after — an account bound to cluster-admin is not something to discover from
// a condition message.

// The catalogue and, for each entry, what the platform has done to it. The
// two are fetched together so the screen is one answer with one age: an
// addon's installed versions are singular and current, and the history beside
// them is the only thing that says they ever moved.
const { data, error, loading, refresh } = useAsync(async () => {
  const catalogue = await api.addons();
  const histories: Record<string, AddonUpgrades> = {};
  await Promise.all(
    catalogue.items.map(async (addon) => {
      try {
        histories[addon.id] = await api.addonUpgrades(addon.id);
      } catch (err) {
        // A history that cannot be read leaves the row's other answers
        // standing. An expired session is not that, and belongs to the shell.
        if (err instanceof APIError && err.status === 401) throw err;
      }
    }),
  );
  return { items: catalogue.items, histories };
});

const busy = ref("");
const failure = ref("");
const removing = ref<Addon | null>(null);
const confirming = ref(false);
const confirmation = ref("");

function askToRemove(addon: Addon) {
  removing.value = addon;
  confirmation.value = "";
  confirming.value = true;
}

const addons = computed(() => data.value?.items ?? []);

// What the platform has upgraded this entry from and to. Undefined where the
// history could not be read at all, which the row says rather than passing
// off as "nothing happened".
function history(addon: Addon): AddonUpgrades | undefined {
  return data.value?.histories[addon.id];
}

function upgrades(addon: Addon): AddonUpgrade[] {
  return history(addon)?.items ?? [];
}

// One side of a transition, as versions rather than as chart names: the row
// above already says which charts the entry installs.
function transition(charts: AddonUpgrade["from"]): string {
  if (!charts?.length) return "unknown";
  return charts.map((chart) => chart.version || "unknown").join(" + ");
}

function ready(addon: Addon) {
  return (addon.conditions ?? []).find((condition) => condition.type === "Ready");
}

// What the row says in one word. Serving is the question that matters —
// whoever installed it — and the rest qualifies it.
function state(addon: Addon): { label: string; tone: "success" | "warning" | "error" | "neutral" } {
  const condition = ready(addon);
  if (addon.serving) {
    return { label: addon.managed ? "installed by the platform" : "already in the cluster", tone: "success" };
  }
  if (condition?.reason === "Installing" || condition?.reason === "Uninstalling") {
    return { label: condition.reason.toLowerCase(), tone: "warning" };
  }
  if (condition && conditionSeverity(condition) === "error") {
    return { label: "not serving", tone: "error" };
  }
  return { label: "not installed", tone: "neutral" };
}

function versions(addon: Addon): string {
  const charts = addon.installed?.length ? addon.installed : addon.charts;
  return charts.map((chart) => `${chart.name} ${chart.version}`).join(", ");
}

async function write(addon: Addon, install: boolean) {
  busy.value = addon.id;
  failure.value = "";
  try {
    if (addon.requested === undefined) {
      await api.createAddon({ id: addon.id, install });
    } else {
      await api.updateAddon(addon.id, { install });
    }
    await refresh();
  } catch (err) {
    failure.value = err instanceof APIError ? err.message : String(err);
  } finally {
    busy.value = "";
  }
}

async function remove() {
  const addon = removing.value;
  if (!addon) return;
  busy.value = addon.id;
  failure.value = "";
  try {
    await api.deleteAddon(addon.id);
    confirming.value = false;
    removing.value = null;
    confirmation.value = "";
    await refresh();
  } catch (err) {
    failure.value = err instanceof APIError ? err.message : String(err);
  } finally {
    busy.value = "";
  }
}
</script>

<template>
  <div class="space-y-6">
    <PageHeader
      title="Addons"
      :breadcrumb="[{ label: 'Platform', to: '/platform' }, { label: 'Addons' }]"
    >
      <template #description>
        The dependencies this platform can install into its own cluster, and what is there now.
      </template>
      <template #actions>
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="loading"
          aria-label="Refresh"
          @click="refresh"
        />
      </template>
    </PageHeader>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <UAlert v-if="failure" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="failure" />

    <PageSection
      title="Catalogue"
      description="Compiled into the operator. An addon names an entry and a namespace and nothing else — its install job can apply CRDs and ClusterRoles, so it may not name a chart."
    >
      <div class="rounded-md border border-default divide-y divide-muted">
        <div v-for="addon in addons" :key="addon.id" class="px-4 py-4 space-y-3">
          <div class="flex items-start justify-between gap-4">
            <div class="min-w-0">
              <div class="flex items-center gap-2">
                <StatusDot :tone="state(addon).tone" class="shrink-0" />
                <span class="text-sm font-medium text-highlighted">{{ addon.title }}</span>
                <span class="font-mono text-[11px] text-dimmed">{{ addon.id }}</span>
              </div>
              <p class="text-xs text-muted mt-1 leading-relaxed">{{ addon.summary }}</p>
            </div>
            <div class="flex items-center gap-2 shrink-0">
              <UButton
                v-if="addon.permitted && !addon.requested"
                size="xs"
                color="primary"
                :loading="busy === addon.id"
                @click="write(addon, true)"
              >
                Install
              </UButton>
              <UButton
                v-else-if="addon.permitted"
                size="xs"
                color="neutral"
                variant="subtle"
                :loading="busy === addon.id"
                @click="write(addon, false)"
              >
                Stop installing
              </UButton>
              <UButton
                v-if="addon.requested !== undefined"
                size="xs"
                color="error"
                variant="ghost"
                icon="i-lucide-trash-2"
                aria-label="Remove"
                @click="askToRemove(addon)"
              />
            </div>
          </div>

          <div class="flex flex-wrap items-center gap-x-6 gap-y-1 text-[11px] text-dimmed">
            <span>{{ state(addon).label }}</span>
            <span class="font-mono">{{ versions(addon) }}</span>
            <span class="font-mono">{{ addon.namespace || addon.defaultNamespace }}</span>
            <span v-if="addon.dependsOn?.length">needs {{ addon.dependsOn.join(", ") }}</span>
          </div>

          <!-- What the platform did to it, and when. The versions above are
               singular and current: without this, an upgrade that broke
               something an hour later leaves a number that has always said
               what it says now. An empty history under a date is "nothing has
               moved since then"; one with no date at all is an installation
               older than the records, and says so. -->
          <div v-if="upgrades(addon).length" class="space-y-1">
            <p class="text-[11px] font-medium text-muted">Upgrades</p>
            <table class="w-full text-xs">
              <thead>
                <tr class="text-left text-muted">
                  <th class="py-1 pr-4 font-medium">When</th>
                  <th class="py-1 pr-4 font-medium">From</th>
                  <th class="py-1 pr-4 font-medium">To</th>
                  <th class="py-1 font-medium">Outcome</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="upgrade in upgrades(addon)"
                  :key="upgrade.name"
                  class="border-t border-default/50"
                >
                  <td class="py-1 pr-4 text-toned" :title="exactTime(upgrade.startedAt)">
                    {{ timeAgo(upgrade.startedAt) }}
                  </td>
                  <td class="py-1 pr-4 font-mono text-dimmed">{{ transition(upgrade.from) }}</td>
                  <td class="py-1 pr-4 font-mono text-highlighted">{{ transition(upgrade.to) }}</td>
                  <td class="py-1" :class="upgrade.phase === 'Failed' ? 'text-error' : 'text-toned'">
                    {{ (upgrade.phase ?? "").toLowerCase() }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <!-- Said only where an upgrade is a thing that could have happened:
               the platform upgrades what it installed and nothing else. -->
          <p v-else-if="addon.managed && history(addon)?.recordedSince" class="text-[11px] text-dimmed">
            No upgrades recorded since {{ exactTime(history(addon)?.recordedSince ?? undefined) }}.
          </p>
          <p v-else-if="addon.managed && history(addon)" class="text-[11px] text-dimmed">
            No upgrade history for this entry yet.
          </p>

          <!-- The grant, before it is made. An account bound to cluster-admin
               is not something to find out about from a condition. -->
          <UAlert
            v-if="!addon.permitted"
            color="neutral"
            variant="soft"
            icon="i-lucide-lock"
            title="This installation has not permitted it"
            :description="`Upgrade the chart with --set ${addon.chartValue}=true, which creates the install job's ServiceAccount${addon.clusterAdmin ? ' — bound to cluster-admin, because ' + addon.grantBecause : ''}.`"
          />

          <p
            v-else-if="ready(addon) && ready(addon)!.status !== 'True'"
            class="text-xs text-muted leading-relaxed"
          >
            {{ ready(addon)!.message }}
          </p>
        </div>

        <div v-if="!addons.length && !loading" class="px-4 py-8 text-center text-xs text-muted">
          This operator has no addons in its catalogue.
        </div>
      </div>
    </PageSection>

    <!-- Removing one is the largest blast radius on this screen, so it is
         confirmed by typing the entry's name — the same shape as deleting a
         project. What is refused rather than confirmed is a dependent: the
         operator answers that one on the addon itself. -->
    <UModal v-model:open="confirming" :title="`Remove ${removing?.title ?? ''}?`">
      <template #body>
        <div v-if="removing" class="space-y-3">
          <UAlert
            color="warning"
            variant="soft"
            icon="i-lucide-triangle-alert"
            :title="removing.managed ? 'This uninstalls the release' : 'This removes the record, not the release'"
            :description="
              removing.managed
                ? removing.blastRadius
                : 'The platform did not install it, so it will not remove it. Only the platform\'s record of it goes.'
            "
          />
          <p class="text-xs text-muted leading-relaxed">
            It is refused while a connection or a claim still provisions through it, and the addon says which.
          </p>
          <UInput v-model="confirmation" :placeholder="removing.id" size="sm" />
          <p class="text-[11px] text-dimmed">Type <span class="font-mono">{{ removing.id }}</span> to confirm.</p>
        </div>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="ghost" size="sm" @click="confirming = false">Cancel</UButton>
          <UButton
            color="error"
            size="sm"
            :disabled="confirmation !== removing?.id"
            :loading="busy === removing?.id"
            @click="remove"
          >
            Remove
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
