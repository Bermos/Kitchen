<script setup lang="ts">
import { api } from "../lib/api";
import { timeAgo } from "../lib/format";
import { operatorMode } from "../lib/mode";
import { unhealthyConditions } from "../lib/status";
import { useAsync } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";
import StatusDot from "../components/StatusDot.vue";

// Connections are plugin instances: git sources, registries, database
// providers. The API never exposes their credentials, so neither can this
// page — creating one stays `kubectl apply` until the create flow lands.

const { data, error, loading, refresh } = useAsync(() => api.connections());
</script>

<template>
  <div class="space-y-6">
    <div class="flex items-center justify-between">
      <div>
        <h1 class="text-xl font-semibold text-highlighted">Connections</h1>
        <p class="text-xs text-muted mt-1">
          Git providers, registries and databases the platform talks to. Credentials never leave the operator.
        </p>
      </div>
      <UButton
        icon="i-lucide-refresh-cw"
        color="neutral"
        variant="ghost"
        size="sm"
        :loading="loading"
        aria-label="Refresh"
        @click="refresh"
      />
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <p v-if="data && !data.length" class="text-sm text-muted py-8 text-center">
      No connections yet — they are created with kubectl until the create flow lands here.
    </p>

    <div v-for="connection in data ?? []" :key="connection.name" class="rounded-md border border-default bg-muted">
      <div class="px-4 py-3 flex items-center gap-3 flex-wrap">
        <StatusDot :tone="unhealthyConditions(connection.conditions).length ? 'error' : 'success'" />
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
      </div>
      <div v-if="operatorMode" class="px-4 pb-3">
        <ConditionsTable :conditions="connection.conditions" />
      </div>
    </div>
  </div>
</template>
