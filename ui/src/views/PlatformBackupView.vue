<script setup lang="ts">
import { computed, ref } from "vue";
import { api, APIError, downloadBackup } from "../lib/api";
import { useAsync } from "../lib/useAsync";
import StatusDot from "../components/StatusDot.vue";

// Backing the platform up: what an archive would carry, what it deliberately
// would not, and the button that takes one.
//
// There is no restore control on this screen, and that is the design rather
// than a gap. A restore happens into a cluster whose accounts database is gone
// — and the credentials to log in here are inside the archive, so there is
// nobody left to press anything. Restore is a Job the chart renders, which
// puts it in the same category as installing the chart and following the
// bootstrap link. docs/BACKUP.md is the procedure.
//
// The exclusions are served by the API rather than written here, so this screen
// and the archive's own manifest cannot come to disagree about what is missing.

const { data, error, loading, refresh } = useAsync(() => api.backup());

const taking = ref(false);
const failure = ref("");
const takenAt = ref("");

const resources = computed(() => {
  const counts = data.value?.resources ?? {};
  return Object.entries(counts)
    .filter(([, count]) => count > 0)
    .sort(([a], [b]) => a.localeCompare(b));
});

const objects = computed(() =>
  Object.values(data.value?.resources ?? {}).reduce((total, count) => total + count, 0),
);

async function take() {
  taking.value = true;
  failure.value = "";
  try {
    const { blob, filename } = await downloadBackup();
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = filename;
    link.click();
    URL.revokeObjectURL(url);
    takenAt.value = filename;
    await refresh();
  } catch (err) {
    failure.value = err instanceof APIError ? err.message : String(err);
  } finally {
    taking.value = false;
  }
}
</script>

<template>
  <div class="space-y-5">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <div class="flex items-center gap-2 text-xs text-muted mb-1">
          <RouterLink to="/platform" class="hover:text-highlighted">Platform</RouterLink>
          <span>/</span>
          <span class="text-toned">Backup</span>
        </div>
        <h1 class="text-xl font-semibold text-highlighted">Backup</h1>
        <p class="text-xs text-muted mt-1">
          One archive: every Kitchen object, every credential in the platform namespace, and the identity provider's
          database.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="loading"
          aria-label="Refresh"
          @click="refresh"
        />
        <UButton icon="i-lucide-download" color="primary" size="sm" :loading="taking" :disabled="!data" @click="take">
          Take a backup
        </UButton>
      </div>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <UAlert v-if="failure" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="failure" />

    <!-- The archive is a credential in its own right, and saying so once here
         is cheaper than explaining it after somebody has left one in a shared
         drive. -->
    <UAlert
      v-if="takenAt"
      color="warning"
      variant="soft"
      icon="i-lucide-shield-alert"
      title="The archive is a credential"
      :description="`${takenAt} holds every secret this platform has, in the clear. Keep it where you would keep the cluster's root credentials, and off the cluster it came from.`"
    />

    <template v-if="data">
      <div class="grid grid-cols-2 sm:grid-cols-4 gap-3">
        <div class="rounded-md border border-default px-4 py-3">
          <p class="text-xs text-muted">Objects</p>
          <p class="text-lg font-semibold text-highlighted tabular-nums mt-1">{{ objects }}</p>
          <p class="text-[11px] text-dimmed mt-0.5">projects, releases, environments</p>
        </div>
        <div class="rounded-md border border-default px-4 py-3">
          <p class="text-xs text-muted">Secrets</p>
          <p class="text-lg font-semibold text-highlighted tabular-nums mt-1">{{ data.secrets }}</p>
          <p class="text-[11px] text-dimmed mt-0.5">in the platform namespace</p>
        </div>
        <div
          class="rounded-md border px-4 py-3"
          :class="data.accounts.available ? 'border-default' : 'border-warning/40 bg-warning/5'"
        >
          <p class="text-xs text-muted">Accounts</p>
          <p
            class="text-lg font-semibold tabular-nums mt-1"
            :class="data.accounts.available ? 'text-highlighted' : 'text-warning'"
          >
            {{ data.accounts.available ? data.accounts.database : "not included" }}
          </p>
          <p class="text-[11px] text-dimmed mt-0.5">the identity provider's database</p>
        </div>
        <div class="rounded-md border border-default px-4 py-3">
          <p class="text-xs text-muted">Release</p>
          <p class="text-lg font-semibold text-highlighted tabular-nums mt-1">{{ data.platformVersion }}</p>
          <p class="text-[11px] text-dimmed mt-0.5">what it restores into</p>
        </div>
      </div>

      <!-- An installation with no identity provider has no accounts to take,
           which is not a fault; one whose database cannot be reached is. The
           message is the API's, and it says which. -->
      <UAlert
        v-if="!data.accounts.available"
        color="warning"
        variant="soft"
        icon="i-lucide-database"
        title="This archive would carry no accounts"
        :description="data.accounts.message"
      />

      <div class="grid gap-4 lg:grid-cols-2">
        <div>
          <h2 class="text-sm font-medium text-highlighted mb-2">What it carries</h2>
          <div class="rounded-md border border-default divide-y divide-muted">
            <div v-for="[kind, count] in resources" :key="kind" class="flex items-center justify-between px-4 py-2">
              <span class="text-xs text-toned">{{ kind }}</span>
              <span class="font-mono text-xs tabular-nums text-highlighted">{{ count }}</span>
            </div>
            <div v-if="!resources.length" class="px-4 py-6 text-center text-xs text-muted">
              This platform holds no objects yet.
            </div>
            <div class="flex items-center justify-between px-4 py-2">
              <span class="text-xs text-toned">secrets</span>
              <span class="font-mono text-xs tabular-nums text-highlighted">{{ data.secrets }}</span>
            </div>
          </div>
          <p class="text-[11px] text-dimmed mt-2 leading-relaxed">
            The credentials are the part that matters most and the part easiest to leave out. A restore without the
            Cloudflare token, the git app keys and the identity provider's own signing secret brings back a platform
            that cannot talk to anything.
          </p>
        </div>

        <div>
          <h2 class="text-sm font-medium text-highlighted mb-2">What it does not</h2>
          <ul class="rounded-md border border-default divide-y divide-muted">
            <li v-for="item in data.excluded" :key="item" class="px-4 py-2 text-xs text-muted leading-relaxed">
              {{ item }}
            </li>
          </ul>
        </div>
      </div>

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">Volume snapshots</h2>
        <div
          class="rounded-md border px-4 py-3"
          :class="data.snapshots.supported ? 'border-default' : 'border-default bg-elevated/30'"
        >
          <p class="text-xs flex items-start gap-2" :class="data.snapshots.supported ? 'text-toned' : 'text-muted'">
            <StatusDot :tone="data.snapshots.supported ? 'success' : 'neutral'" class="mt-1 shrink-0" />
            <span v-if="data.snapshots.supported">
              This cluster can snapshot volumes, through
              <span class="font-mono">{{ (data.snapshots.classes ?? []).join(", ") }}</span
              >. That is the cheap answer for the two volumes this archive cannot carry — the accounts database and the
              telemetry store — and it is an option rather than the plan: a snapshot lives on the same storage as the
              volume it copies.
            </span>
            <span v-else>{{ data.snapshots.message }}</span>
          </p>
        </div>
      </div>

      <div class="rounded-md border border-default px-4 py-3">
        <h2 class="text-sm font-medium text-highlighted mb-1">Restoring</h2>
        <p class="text-xs text-muted leading-relaxed">
          There is no restore button, and there cannot be one: a restore happens into a cluster whose accounts database
          is gone, so there is nobody left to log in with. The chart renders a Job for it instead — the same category as
          installing the chart and following the bootstrap link. An archive restores into the release that wrote it
          ({{ data.platformVersion }}); upgrade afterwards, not before. The procedure, and the CI job that runs it on
          every change, are in <span class="font-mono">docs/BACKUP.md</span>.
        </p>
      </div>
    </template>
    <p v-else-if="loading" class="text-xs text-muted">Loading…</p>
  </div>
</template>
