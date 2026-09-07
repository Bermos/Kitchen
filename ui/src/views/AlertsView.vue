<script setup lang="ts">
import { computed } from "vue";
import { api } from "../lib/api";
import { alertsSentence } from "../lib/alerts";
import { useFreshness } from "../lib/freshness";
import { may } from "../lib/policy";
import { callerFor } from "../lib/me";
import { useAsync, usePoll } from "../lib/useAsync";
import AlertList from "../components/AlertList.vue";
import PageHeader from "../components/PageHeader.vue";

// The fleet's third question: what needs acting on, across everything.
//
// Overview answers what exists and Deploys answers what shipped; this answers
// what is asking for somebody. It is in the Fleet scope rather than the
// Platform one because **both audiences have this screen** — the API answers
// it for anybody with a token and narrows it to what they may see. A member
// gets their projects' own rows, plus the symptom rows they are owed about a
// platform condition degrading them; an operator gets every delivery of both
// audiences, which on this platform is the whole estate in one list.
//
// It is deliberately not a second problems list. `/platform/signals` answers
// *what is wrong*; this answers *and what am I meant to do about it*, which is
// a question only the pair (condition, audience) has an answer to — the same
// condition is urgent for the person who can fix it and a data point for the
// person who cannot.

const { data, error, loading, refresh } = useAsync(() => api.alerts());
const freshness = useFreshness();
usePoll(() => void refresh(), 30000, () => true);

const headline = computed(() => alertsSentence(data.value?.items));

// Claiming is the operators' as a group, so the button is offered to whoever
// the API would accept it from rather than shown and refused.
const canClaim = computed(() => may("POST /api/v1/alerts/claim", callerFor()));
</script>

<template>
  <div class="space-y-6">
    <PageHeader :freshness="freshness" title="Alerts">
      <template #description>
        Everything asking for somebody, worst first — and what is already being done about each one.
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
      empty="Nothing needs acting on. Every rule in the catalogue was evaluated and none of them matched."
      @changed="refresh"
    />
  </div>
</template>
