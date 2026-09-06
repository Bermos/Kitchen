<script setup lang="ts">
import { computed, watch } from "vue";
import { useRoute } from "vue-router";
import { api, type SignalsAnswer } from "../lib/api";
import { useFreshness } from "../lib/freshness";
import { problemsSentence } from "../lib/signals";
import { useAsync, usePoll } from "../lib/useAsync";
import FindingList from "../components/FindingList.vue";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";

// What is firing about this project, with the evidence beside it.
//
// The catalogue is evaluated per environment (`GET /environments/{name}/signals`
// — a viewer's route, so this is every member's screen), and a project is its
// environments, so this screen is one round per environment merged into one
// answer. Merged rather than stacked: five collapsed panels, four of them
// empty, is the shape a triage cannot scan, and each finding already names the
// environment it is about.
//
// **An empty list and an unread input are different answers**, which is the
// whole reason `SignalsAnswer` carries `unreadable` — a platform reporting no
// problems because it could not check anything is the failure this design
// exists to prevent. Merging preserves it: an input that failed for one
// environment is named with that environment, and the clean sentence is only
// said when nothing went unread anywhere.
//
// The tier a finding fires at, what is mitigating it, and the project-scoped
// silence control are #471's and are deliberately not built here — the note
// below the list says where they will be, rather than a column of dashes
// pretending to be one.

const route = useRoute();
const name = computed(() => route.params.name as string);

const { data, error, loading, refresh } = useAsync(async () => {
  const environments = await api.projectEnvironments(name.value);
  // One environment's evaluation failing is not the screen failing. An
  // environment whose round could not be asked for at all is named in
  // `unreadable`, which is exactly what that field is for.
  const rounds = await Promise.all(
    environments.map(async (environment) => {
      try {
        return { environment: environment.name, answer: await api.environmentSignals(environment.name) };
      } catch (err) {
        return { environment: environment.name, reason: err instanceof Error ? err.message : String(err) };
      }
    }),
  );
  return { environments, rounds };
});
watch(name, () => void refresh());
// How old this screen is, and the reader's hold on it: every fetch above
// reports into it and the header renders it.
const freshness = useFreshness();
usePoll(() => void refresh(), 30000, () => true);

/** Every environment's round as one. The evaluation time is the oldest of
 * them, on the same principle the freshness control uses: the reader is asking
 * whether they can trust what is in front of them, and the weakest part is the
 * honest answer. */
const answer = computed<SignalsAnswer | null>(() => {
  const rounds = data.value?.rounds;
  if (!rounds?.length) return null;
  const merged: SignalsAnswer = {
    items: [],
    counts: { critical: 0, warning: 0, info: 0 },
    unreadable: [],
    evaluatedAt: "",
    project: name.value,
  };
  for (const round of rounds) {
    if (!round.answer) {
      merged.unreadable!.push({
        input: round.environment,
        reason: round.reason ?? "the catalogue could not be evaluated for this environment",
      });
      continue;
    }
    merged.items.push(...round.answer.items);
    merged.counts.critical += round.answer.counts.critical;
    merged.counts.warning += round.answer.counts.warning;
    merged.counts.info += round.answer.counts.info;
    for (const failure of round.answer.unreadable ?? []) {
      merged.unreadable!.push({ input: `${round.environment}/${failure.input}`, reason: failure.reason });
    }
    if (!merged.evaluatedAt || round.answer.evaluatedAt < merged.evaluatedAt) {
      merged.evaluatedAt = round.answer.evaluatedAt;
    }
  }
  return merged;
});

const headline = computed(() => {
  if (!data.value) return "";
  if (!data.value.environments.length) return "Nothing is published yet, so there is nothing to evaluate.";
  return problemsSentence(answer.value?.counts);
});
</script>

<template>
  <div class="space-y-6">
    <PageHeader
      :freshness="freshness"
      title="Alerts"
      :breadcrumb="[{ label: 'Projects', to: '/projects' }, { label: name, mono: true }, { label: 'Alerts' }]"
    >
      <template #description>
        Every rule in the catalogue, evaluated against each of this project's environments — and the screen that shows
        the numbers behind each one.
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

    <p v-if="headline" class="text-sm text-muted">{{ headline }}</p>

    <FindingList
      :answer="answer"
      :loading="loading"
      title="Firing"
      empty="Nothing is firing. Every rule in the catalogue was evaluated against every environment of this project and none of them matched."
    />

    <!-- The place two things are going, said rather than mocked up: a column
         of dashes would read as a feature that is broken instead of one that
         has not landed. -->
    <PageSection
      title="Tier, mitigation and silence"
      description="Not built yet, and shaped here so that it lands in one place when it is."
    >
      <div class="rounded-md border border-dashed border-default px-4 py-3 text-xs text-muted space-y-1">
        <p>
          Each finding will carry <span class="text-toned">the tier it fires at</span> and
          <span class="text-toned">what is already mitigating it</span>, so that a warning something is holding back
          reads differently from one nothing is — and a silence control scoped to this project, so that a known
          condition can be quietened here rather than everywhere.
        </p>
        <p>
          They are one design decision with the fleet's own alerts screen and belong to it, not to this screen: the
          same finding must not say two different things depending on which list it is read from.
        </p>
      </div>
    </PageSection>
  </div>
</template>
