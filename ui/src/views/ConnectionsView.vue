<script setup lang="ts">
import { ref } from "vue";
import { api, type Connection } from "../lib/api";
import { timeAgo } from "../lib/format";
import { conditionsTone } from "../lib/status";
import { useAsync } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";
import ConnectionModal from "../components/ConnectionModal.vue";
import PageHeader from "../components/PageHeader.vue";
import StatusDot from "../components/StatusDot.vue";

// Connections are plugin instances: git sources, registries, database
// providers. The API never exposes their credentials — creating one sends the
// credential to the operator, which stores it and never reads it back.

const toast = useToast();
const { data, error, loading, refresh } = useAsync(() => api.connections());

const deleteTarget = ref<Connection | null>(null);
const deleting = ref(false);
async function deleteConnection() {
  const target = deleteTarget.value;
  if (!target) return;
  deleting.value = true;
  try {
    await api.deleteConnection(target.name);
    toast.add({ title: `Connection ${target.name} deleted`, color: "success", icon: "i-lucide-trash-2" });
    deleteTarget.value = null;
    await refresh();
  } catch (err) {
    toast.add({
      title: "Deleting the connection failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    deleting.value = false;
  }
}
</script>

<template>
  <div class="space-y-6">
    <PageHeader title="Connections">
      <template #description>
        Git providers, registries and databases the platform talks to. Credentials never leave the operator.
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
        <ConnectionModal @saved="refresh" />
      </template>
    </PageHeader>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <p v-if="data && !data.length" class="text-sm text-muted py-8 text-center">
      No connections yet — create one to link a git provider or a registry.
    </p>

    <div v-for="connection in data ?? []" :key="connection.name" class="rounded-md border border-default bg-muted">
      <div class="px-4 py-3 flex items-center gap-3 flex-wrap">
        <StatusDot :tone="conditionsTone(connection.conditions)" />
        <span class="text-highlighted font-medium">{{ connection.name }}</span>
        <UBadge color="neutral" variant="subtle" size="sm" class="font-mono">{{ connection.provider }}</UBadge>
        <span class="flex-1" />
        <UBadge
          v-for="capability in connection.capabilities ?? []"
          :key="capability"
          color="primary"
          variant="soft"
          size="sm"
          class="font-mono"
          >{{ capability }}</UBadge
        >
        <span class="text-xs text-muted">{{ timeAgo(connection.createdAt) }}</span>
        <ConnectionModal :connection="connection" @saved="refresh">
          <UButton color="neutral" variant="ghost" size="xs" icon="i-lucide-key-round" aria-label="Edit" />
        </ConnectionModal>
        <UButton
          color="neutral"
          variant="ghost"
          size="xs"
          icon="i-lucide-trash-2"
          aria-label="Delete"
          @click="deleteTarget = connection"
        />
      </div>
      <!-- Why the dot is not green, in full. Managing a connection is the
           operator's outright — this whole screen is behind that role — so
           the conditions are not gated on the mode a second time. -->
      <div v-if="connection.conditions?.length" class="px-4 pb-3">
        <ConditionsTable :conditions="connection.conditions" />
      </div>
    </div>

    <!-- Delete confirmation. The API refuses a connection that is still in
         use and names what uses it, so the dangerous case explains itself. -->
    <UModal
      :open="deleteTarget !== null"
      :title="`Delete ${deleteTarget?.name}?`"
      description="The stored credential is deleted with it. Projects using this connection would stop building — the API refuses the deletion while any do."
      @update:open="(open: boolean) => { if (!open) deleteTarget = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="deleteTarget = null">Cancel</UButton>
          <UButton color="error" :loading="deleting" icon="i-lucide-trash-2" @click="deleteConnection">
            Delete {{ deleteTarget?.name }}
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
