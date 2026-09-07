<script setup lang="ts">
import { computed, watch } from "vue";
import { useRoute } from "vue-router";
import { api } from "../lib/api";
import { alertsSentence } from "../lib/alerts";
import { useFreshness } from "../lib/freshness";
import { may } from "../lib/policy";
import { callerFor } from "../lib/me";
import { useAsync, usePoll } from "../lib/useAsync";
import AlertList from "../components/AlertList.vue";
import PageHeader from "../components/PageHeader.vue";

// What is asking for somebody about this project, and what is already being
// done about it.
//
// It is the fleet's Alerts screen narrowed to one project — the same rows, the
// same component, the same controls. That is the point rather than a saving:
// **the same condition must not say two different things depending on which
// list it is read from**, and until this screen and the fleet's were one list
// with one filter they were two screens that could drift.
//
// Two things on it are only here. A **symptom** row is a platform condition
// degrading this project, in the project's own words — it is what the platform
// owes a developer who cannot act on the cause and whose production is
// nevertheless degraded. And the **quieten** control is project-scoped: it
// quietens the row this project reads and never the one the operator reads
// about the same condition, which is the whole of why an acknowledgement and a
// silence are keyed on the delivery rather than on the condition.

const route = useRoute();
const name = computed(() => route.params.name as string);

const { data, error, loading, refresh } = useAsync(() => api.alerts({ project: name.value }));
watch(name, () => void refresh());
const freshness = useFreshness();
usePoll(() => void refresh(), 30000, () => true);

const headline = computed(() => alertsSentence(data.value?.items));
const canClaim = computed(() => may("POST /api/v1/alerts/claim", callerFor()));
</script>

<template>
  <div class="space-y-6">
    <PageHeader
      :freshness="freshness"
      title="Alerts"
      :breadcrumb="[{ label: 'Projects', to: '/projects' }, { label: name, mono: true }, { label: 'Alerts' }]"
    >
      <template #description>
        Everything the catalogue is saying about this project, at the tier it is asking you to act at — and what is
        already being done about each one.
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

    <p v-if="data" class="text-sm text-muted">{{ headline }}</p>

    <AlertList
      :answer="data"
      :loading="loading"
      :error="error"
      :can-claim="canClaim"
      empty="Nothing needs acting on. Every rule in the catalogue was evaluated against this project and none of them matched."
      @changed="refresh"
    />
  </div>
</template>
