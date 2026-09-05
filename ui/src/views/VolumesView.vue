<script setup lang="ts">
import { ref } from "vue";
import { api, type PlatformWrittenVolume } from "../lib/api";
import { timeAgo } from "../lib/format";
import { useAsync } from "../lib/useAsync";
import PageHeader from "../components/PageHeader.vue";
import StatusDot from "../components/StatusDot.vue";
import VolumeModal from "../components/VolumeModal.vue";

// The storage an operator points the platform at: an NFS export on a NAS
// older than the cluster, a volume a storage appliance's driver hands out. A
// project mounts one with a `bind` volume claim, and until this screen
// existed the object behind that claim was the one step of the whole platform
// that needed kubectl.
//
// It sits beside Connections rather than under /platform because it has a
// connection's standing: cluster-scoped, written once by whoever administers
// the installation, and pointed at by projects afterwards. Like Connections
// it is the operator's entire, so nothing inside it is gated a second time —
// docs/UI.md, "The gate is per screen, not per block".
//
// The screen lists what the platform wrote and only that. A PersistentVolume
// somebody applied by hand is still bindable and still appears in the claim
// form; it is simply not something the platform is accountable for, and a
// list that mixed the two would be claiming otherwise.

const toast = useToast();
const { data, error, loading, refresh } = useAsync(() => api.platformWrittenVolumes());

const deleteTarget = ref<PlatformWrittenVolume | null>(null);
const deleting = ref(false);
async function deleteVolume() {
  const target = deleteTarget.value;
  if (!target) return;
  deleting.value = true;
  try {
    await api.deletePlatformWrittenVolume(target.name);
    toast.add({ title: `Volume ${target.name} removed`, color: "success", icon: "i-lucide-trash-2" });
    deleteTarget.value = null;
    await refresh();
  } catch (err) {
    toast.add({
      title: "Removing the volume failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    deleting.value = false;
  }
}

/** What the volume points at, in the words somebody checking it would use. */
function points(volume: PlatformWrittenVolume): string {
  if (volume.nfs) return `${volume.nfs.server}:${volume.nfs.path}`;
  if (volume.csi) return `${volume.csi.driver} · ${volume.csi.volumeHandle}`;
  return volume.identity ?? "—";
}

/** Available, Bound, Released — the volume's own state. A volume nothing has
 * claimed yet is the ordinary case here, not a fault. */
function tone(volume: PlatformWrittenVolume): "success" | "warning" | "neutral" {
  if (volume.phase === "Failed") return "warning";
  return volume.phase === "Bound" ? "success" : "neutral";
}
</script>

<template>
  <div class="space-y-6">
    <PageHeader title="Volumes">
      <template #description>
        Storage that existed before this platform did — an NFS export, a volume a storage driver already knows about —
        written so that a project can mount it with a volume claim. The platform never formats one and never deletes
        the data on one.
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
        <VolumeModal @saved="refresh" />
      </template>
    </PageHeader>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <p v-if="data && !data.length" class="text-sm text-muted py-8 text-center">
      No volumes yet — write one to let a project mount an export or a share that already holds data.
    </p>

    <div v-for="volume in data ?? []" :key="volume.name" class="rounded-md border border-default bg-muted">
      <div class="px-4 py-3 flex items-center gap-3 flex-wrap">
        <StatusDot :tone="tone(volume)" />
        <span class="text-highlighted font-medium font-mono">{{ volume.name }}</span>
        <UBadge color="neutral" variant="subtle" size="sm" class="font-mono">{{ volume.type || "—" }}</UBadge>
        <span class="font-mono text-xs text-muted break-all">{{ points(volume) }}</span>
        <span class="flex-1" />
        <UBadge v-if="volume.capacity" color="neutral" variant="soft" size="sm" class="font-mono">{{
          volume.capacity
        }}</UBadge>
        <UBadge
          v-for="mode in volume.accessModes ?? []"
          :key="mode"
          color="primary"
          variant="soft"
          size="sm"
          class="font-mono"
          >{{ mode }}</UBadge
        >
        <span v-if="volume.createdAt" class="text-xs text-muted">{{ timeAgo(volume.createdAt) }}</span>
        <UButton
          color="neutral"
          variant="ghost"
          size="xs"
          icon="i-lucide-trash-2"
          aria-label="Remove"
          @click="deleteTarget = volume"
        />
      </div>

      <div class="px-4 pb-3 space-y-1">
        <p class="text-xs text-muted">
          <span class="text-toned">{{ volume.phase || "Available" }}</span>
          <template v-if="volume.heldBy?.length">
            — mounted by
            <span class="font-mono">{{ volume.heldBy.join(", ") }}</span>
          </template>
          <template v-else-if="volume.claimedBy">
            — held by
            <span class="font-mono">{{ volume.claimedBy.namespace }}/{{ volume.claimedBy.name }}</span
            >, which is not one of this platform's claims
          </template>
          <template v-else> — nothing mounts it yet</template>
        </p>
        <p class="text-[11px] text-dimmed">
          Retains its data: removing it removes the platform's record and leaves every byte where it is.
          <template v-if="volume.csi?.volumeAttributes && Object.keys(volume.csi.volumeAttributes).length">
            Driver settings:
            <span class="font-mono"
              >{{ Object.entries(volume.csi.volumeAttributes).map(([k, v]) => `${k}=${v}`).join(", ") }}</span
            >.
          </template>
        </p>
      </div>
    </div>

    <!-- Removing the record, and nothing on the storage. The API refuses
         while a claim mounts it and names the claim, so the dangerous case
         explains itself; what is left to say here is what removal does not
         do, which is the thing somebody deleting twelve terabytes' worth of
         record needs to read. -->
    <UModal
      :open="deleteTarget !== null"
      :title="`Remove ${deleteTarget?.name}?`"
      description="The platform forgets this storage. Nothing on the server is deleted, formatted or unexported, and the data stays exactly where it is. A project mounting it would lose the mount, so the API refuses while any does."
      @update:open="(open: boolean) => { if (!open) deleteTarget = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="deleteTarget = null">Cancel</UButton>
          <UButton color="error" :loading="deleting" icon="i-lucide-trash-2" @click="deleteVolume">
            Remove {{ deleteTarget?.name }}
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
