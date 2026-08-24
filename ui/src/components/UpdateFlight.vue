<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type PlatformUpdate } from "../lib/api";
import { timeAgo } from "../lib/format";
import { PLATFORM_NAMESPACE, frozen, settled, versionLabel, type UpdateStage } from "../lib/updates";
import { useAsync, usePoll } from "../lib/useAsync";
import ComponentChecklist from "./ComponentChecklist.vue";
import UpdateLogs from "./UpdateLogs.vue";

// One upgrade while it is happening: where it has got to, what it is waiting
// for, what is going wrong, and what helm said.
//
// The card exists because the upgrade is the one operation whose progress
// cannot be reported by the thing doing it — applying the chart replaces the
// operator that serves this API, so for a minute or two nothing authenticated
// answers. The four panels below are ordered by how much they can be trusted
// at that moment: the stage (which knows the API is down and says so), the
// checklist (frozen, and labelled as last seen), the cluster's warnings (also
// frozen), and helm's output (which arrives late but is the only thing that
// explains a failure).

const props = defineProps<{
  update: PlatformUpdate;
  stage: UpdateStage;
  /** The version this page was loaded against. */
  startedOn: string;
  /** The version /config.json reports now. */
  nowOn: string;
}>();

const done = computed(() => settled(props.stage));
const stale = computed(() => frozen(props.stage));

// The component survey, which is what `helm upgrade --wait` is waiting for.
// It is the operator's own status field, so it stops moving exactly when the
// operator does; `seenAt` is stamped on the reads that succeed so the panel can
// say the numbers are the last ones seen rather than the current ones.
const seenAt = ref("");
const status = useAsync(async () => {
  const answer = await api.status();
  seenAt.value = new Date().toISOString();
  return answer;
});
usePoll(() => void status.refresh(), 5000, () => !done.value);
const components = computed(() => status.data.value?.components ?? []);

// What the cluster is complaining about inside the upgrade's own window.
// Warnings only, by design — this is what is going wrong, never a narration of
// what is going right, so it renders nothing at all when nothing is wrong.
const warnings = useAsync(async () => {
  // An update that has not started has no window to ask over, and asking
  // without one would answer with warnings from before it — which would be
  // this panel blaming the upgrade for something that predates it.
  if (!props.update.startedAt) return null;
  return api.platformEvents({ namespace: PLATFORM_NAMESPACE, since: props.update.startedAt, limit: 20 });
});
usePoll(() => void warnings.refresh(), 15000, () => !done.value && !!props.update.startedAt);
const events = computed(() => warnings.data.value?.items ?? []);

/**
 * Where the upgrade is, in the words of somebody watching it happen.
 *
 * The record's own `message` is not folded in here: the operator writes it
 * from the update job's pod — what the kubelet is waiting for, what helm's
 * container said as it died — and it is a different sentence from where the
 * upgrade has got to. The template shows both, so a stage this dashboard is
 * sure about never hides what the platform said underneath it.
 */
const narrative = computed(() => {
  const to = versionLabel(props.update.version);
  const from = versionLabel(props.update.fromVersion || props.startedOn);
  switch (props.stage) {
    case "waiting":
      return {
        icon: "i-lucide-clock",
        tone: "text-dimmed",
        title: `Waiting to start the upgrade to ${to}`,
        description:
          "Nothing has been applied yet. An update waits here while another one finishes, or when a preflight check refused it.",
      };
    case "applying":
      return {
        icon: "i-lucide-loader",
        tone: "text-info",
        title: `Upgrading to ${to}`,
        description:
          "helm is applying the chart and waiting for every component to come back. The operator serving this page is one of them, so the API will stop answering part of the way through.",
      };
    case "restarting":
      return {
        icon: "i-lucide-refresh-cw",
        tone: "text-info",
        title: "The platform is restarting itself",
        description:
          "The API has stopped answering. That is the upgrade replacing the operator that serves it, not a failure — this page is watching /config.json, which the new operator serves without a token, and will say so when it answers. A real failure arrives as a phase once the API is back.",
      };
    case "landed":
      return {
        icon: "i-lucide-plug-zap",
        tone: "text-info",
        title: `${versionLabel(props.nowOn)} is serving`,
        description:
          "The new operator is answering for the static files. Waiting for its API to take requests again, and for the upgrade's own record to come back with it.",
      };
    case "reconnected":
      return {
        icon: "i-lucide-check",
        tone: "text-success",
        title: `Reconnected — now on ${versionLabel(props.nowOn)}`,
        description: "The new operator is serving and the API is answering. Its record of the upgrade settles in a moment.",
      };
    case "succeeded":
      return {
        icon: "i-lucide-circle-check",
        tone: "text-success",
        title: `Upgraded to ${to}`,
        description:
          props.nowOn && props.nowOn !== props.startedOn
            ? `This page is talking to the new operator, on ${versionLabel(props.nowOn)}. Reload it to be sure every screen is the new build.`
            : "helm reported the upgrade complete.",
      };
    default:
      return {
        icon: "i-lucide-triangle-alert",
        tone: "text-error",
        title: `The upgrade to ${to} failed`,
        description: `helm ran with --atomic, so a failure it observed has been rolled back to ${from}. What it said is below, and its whole output is underneath that.`,
      };
  }
});

type StepState = "done" | "active" | "todo";

/** The sequence, as three things that either have happened or have not. It is
 *  written down rather than implied because the middle one — the platform
 *  going away — is the step that reads as a fault when nothing names it. */
const steps = computed<{ key: string; label: string; state: StepState }[]>(() => {
  const stage = props.stage;
  const applied: StepState = stage === "waiting" ? "todo" : stage === "applying" ? "active" : "done";
  const replacing: StepState =
    stage === "waiting" || stage === "applying"
      ? "todo"
      : stage === "restarting"
        ? "active"
        : "done";
  const back: StepState =
    stage === "landed"
      ? "active"
      : stage === "reconnected" || stage === "succeeded"
        ? "done"
        : stage === "failed"
          ? "todo"
          : "todo";
  return [
    { key: "apply", label: "helm applies the chart", state: applied },
    { key: "replace", label: "the operator replaces itself and the API goes quiet", state: replacing },
    {
      key: "back",
      label: props.nowOn && props.nowOn !== props.startedOn ? `back on ${versionLabel(props.nowOn)}` : "back, on the new version",
      state: back,
    },
  ];
});

function stepIcon(state: StepState): string {
  if (state === "done") return "i-lucide-check";
  if (state === "active") return "i-lucide-loader";
  return "i-lucide-circle-dashed";
}

function stepClass(state: StepState): string {
  if (state === "done") return "text-success";
  if (state === "active") return "text-info animate-pulse";
  return "text-dimmed";
}

function eventLink(namespace?: string, kind?: string, name?: string) {
  return { path: "/platform/events", query: { namespace, kind, name } };
}

/** A read that failed while the API was up is worth a line; one that failed
 *  during the blackout is the blackout, and is already said above. */
const surveyMessage = computed(() => {
  if (stale.value) return "The API is not answering just now, so this is the last survey that arrived.";
  return status.error.value ? `The component survey could not be read: ${status.error.value}` : "";
});
</script>

<template>
  <div class="rounded-md border px-4 py-3 space-y-4" :class="stage === 'failed' ? 'border-error/40 bg-error/5' : 'border-info/40 bg-info/5'">
    <div class="flex items-start gap-3">
      <UIcon
        :name="narrative.icon"
        class="size-4 mt-0.5 shrink-0"
        :class="[narrative.tone, stage === 'applying' || stage === 'restarting' ? 'animate-pulse' : '']"
      />
      <div class="min-w-0 space-y-1">
        <p class="text-sm font-medium text-highlighted">{{ narrative.title }}</p>
        <p class="text-xs text-toned leading-relaxed">{{ narrative.description }}</p>
        <!-- What the platform itself last said about this update: the pod's
             waiting reason while it runs, helm's own words when it fails. -->
        <p
          v-if="update.message"
          class="text-[11px] font-mono whitespace-pre-wrap break-words max-h-40 overflow-auto"
          :class="stage === 'failed' ? 'text-error' : 'text-toned'"
        >
          {{ update.message }}
        </p>
        <p class="text-[11px] text-dimmed font-mono">
          {{ update.name }}
          <template v-if="update.startedAt"> · started {{ timeAgo(update.startedAt) }}</template>
          <template v-if="update.requestedBy"> · by {{ update.requestedBy }}</template>
        </p>
      </div>
    </div>

    <!-- The sequence, so the quiet middle of it is an expected step rather
         than the page having broken. -->
    <ul class="space-y-1">
      <li v-for="step in steps" :key="step.key" class="flex items-center gap-2 text-xs">
        <UIcon :name="stepIcon(step.state)" class="size-3.5 shrink-0" :class="stepClass(step.state)" />
        <span :class="step.state === 'todo' ? 'text-dimmed' : 'text-toned'">{{ step.label }}</span>
      </li>
    </ul>

    <div class="rounded border border-default bg-muted px-3 py-2.5">
      <ComponentChecklist :components="components" :seen-at="seenAt" :stale="stale" :message="surveyMessage" />
      <p v-if="surveyMessage && components.length" class="text-[11px] mt-2" :class="stale ? 'text-dimmed' : 'text-warning'">
        {{ surveyMessage }}
      </p>
      <p v-else-if="!surveyMessage" class="text-[11px] text-dimmed mt-2">
        This is what <span class="font-mono">--wait</span> is waiting for. A component short of its pods part of the way
        through an upgrade is the rollout happening, not a fault; one that stays short is what the upgrade is stuck on.
      </p>
    </div>

    <!-- Warnings only, and nothing at all when there are none: this panel says
         what is going wrong, and an empty one would read as progress. -->
    <div v-if="events.length" class="rounded border border-warning/40 bg-warning/5 px-3 py-2.5 space-y-2">
      <div class="flex items-baseline justify-between gap-3 flex-wrap">
        <p class="text-xs text-warning font-medium">What is going wrong</p>
        <p class="text-[11px] text-dimmed">
          <template v-if="stale">last seen · </template>warnings in
          <span class="font-mono">{{ PLATFORM_NAMESPACE }}</span> since the upgrade started
        </p>
      </div>
      <div v-for="(event, i) in events" :key="i" class="text-[11px] leading-relaxed">
        <p>
          <span class="font-mono text-warning">{{ event.reason }}</span>
          <span class="text-dimmed font-mono"> · {{ event.kind }}/{{ event.name }}</span>
          <span class="text-dimmed"> · {{ timeAgo(event.timestamp) }}</span>
          <span v-if="event.count > 1" class="text-dimmed"> · ×{{ event.count }}</span>
        </p>
        <p class="text-toned break-words">{{ event.message }}</p>
        <RouterLink :to="eventLink(event.namespace, event.kind, event.name)" class="text-primary hover:underline">
          its events
        </RouterLink>
      </div>
    </div>

    <!-- The output goes through the API like everything else, so during the
         blackout there is nothing to read it with. Saying so is better than a
         viewer full of failed requests — the lines are kept in the store and
         come back with the API, including the ones written while it was away. -->
    <div>
      <p class="text-xs text-muted mb-2">What helm said</p>
      <UpdateLogs v-if="!stale" :name="update.name" :live="!done" />
      <p v-else class="text-xs text-muted">
        helm's output is read through the API, which is not answering while the operator restarts. It comes back with
        the API, with everything written in the meantime — the job's lines are kept in the log store, not in the job.
      </p>
    </div>
  </div>
</template>
