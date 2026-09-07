<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type SignalPolicy, type SignalPolicyPatch } from "../lib/api";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";
import { confidenceIcon, confidenceLabel, confidenceMeaning } from "../lib/signals";
import { useAsync } from "../lib/useAsync";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";

// What this installation counts as worth hearing.
//
// It is deliberately not the routing screen. `/alerts → Routing` is *who hears
// about a condition*, and a project edits its own; this is *what makes
// something a condition at all*, it is installation-wide, and it is the
// operator's alone. Every number here applies to every project, and the
// compliance posture reads it. Letting a project tighten its own thresholds is
// the other half of #472's design and is not built — #519 — so nothing on this
// screen offers it, because a screen that promised the switch is where
// somebody would go looking for it.
//
// The screen is a choice of three and then the numbers underneath, in that
// order, because that is the decision an operator is actually making. Nobody
// arrives here wanting to set an escalation window; they arrive having decided
// that this platform is a homelab, or that it is not.
//
// The reason a screen exists at all is written on it, and it is not a small
// one: this reverses a decision the repository took deliberately, on the terms
// that decision set. docs/OBSERVABILITY.md §9 recorded the trade-off as "code,
// until the alerting era forces the question with real requirements", and the
// homelab installation is the requirement — a threshold of three correlated
// projects is a threshold a three-project estate never reaches, so the
// detector reads there as health.

const toast = useToast();
const { data, error, loading, refresh } = useAsync(() => api.signalPolicy());

const caller = computed(() => callerFor());
const mayWrite = computed(() => may("PATCH /api/v1/platform/policy", caller.value));
const readOnlyReason = computed(() => refusal("PATCH /api/v1/platform/policy", caller.value));

// The form is a copy of what is served, so that only what moved is sent: the
// route leaves an absent field alone, and sending them all back would make
// every save a change to every number in the audit log.
const form = ref<SignalPolicyPatch>({});
watch(data, (value: SignalPolicy | null) => {
  if (!value) return;
  form.value = {
    correlatedProjects: value.correlatedProjects,
    correlationWindowMinutes: value.correlationWindowMinutes,
    escalationWindowMinutes: value.escalationWindowMinutes,
    untendedMultiple: value.untendedMultiple,
    maxSilenceHours: value.maxSilenceHours,
    paging: value.paging,
  };
});

const moved = computed(() => {
  const value = data.value;
  if (!value) return [] as string[];
  const changed: string[] = [];
  for (const key of Object.keys(form.value) as (keyof SignalPolicyPatch)[]) {
    if (form.value[key] !== (value as unknown as Record<string, unknown>)[key]) changed.push(key);
  }
  return changed;
});

// The numbers, in the order the issue lists them: what counts as one incident,
// then the clock on an unmitigated one, then how quiet a member may go.
const fields = [
  {
    key: "correlatedProjects" as const,
    label: "Projects that make a correlation",
    unit: "projects",
    hint: "How many must be degrading together before it is one platform problem rather than several application problems.",
  },
  {
    key: "correlationWindowMinutes" as const,
    label: "Correlation window",
    unit: "minutes",
    hint: "How far apart two failures may have started and still count as the same moment.",
  },
  {
    key: "escalationWindowMinutes" as const,
    label: "Escalation window",
    unit: "minutes",
    hint: "How long a failure may run with nobody acknowledging it before the operator is added to it. It repeats and adds a ticket; nothing becomes more urgent.",
  },
  {
    key: "untendedMultiple" as const,
    label: "Untended after",
    unit: "× the escalation window",
    hint: "How many of those windows it survives before it stops being an alert and becomes a line on the compliance posture.",
  },
  {
    key: "maxSilenceHours" as const,
    label: "Longest silence",
    unit: "hours",
    hint: "The most a member may quieten their own project's row for. A silence nobody revisits is a decision whose reason has stopped being true without anybody noticing.",
  },
];

const untendedAfter = computed(() => {
  const escalation = form.value.escalationWindowMinutes ?? 0;
  const multiple = form.value.untendedMultiple ?? 0;
  const hours = (escalation * multiple) / 60;
  return hours >= 1 ? `${Math.round(hours * 10) / 10}h` : `${escalation * multiple}m`;
});

const saving = ref(false);

async function save(body: SignalPolicyPatch, title: string) {
  saving.value = true;
  try {
    await api.updateSignalPolicy(body);
    toast.add({ title, color: "success", icon: "i-lucide-check" });
    await refresh();
  } catch (err) {
    // A refusal here names the field, the bound and what the number is for,
    // which is a sentence worth reading in full rather than a title trimmed
    // to fit.
    toast.add({
      title: "The policy was not changed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    saving.value = false;
  }
}

// Choosing a preset clears every override, which is what the word means here:
// picking one on an installation with two numbers moved must not leave a
// policy that is none of the three and looks like one.
function choose(preset: string) {
  void save({ preset }, `Now on the ${preset} policy`);
}

function apply() {
  const body: SignalPolicyPatch = {};
  for (const key of moved.value as (keyof SignalPolicyPatch)[]) {
    (body as Record<string, unknown>)[key] = form.value[key];
  }
  void save(body, "Policy saved");
}

const ladder = [
  { confidence: "coincidence" as const },
  { confidence: "dependency" as const },
  { confidence: "change" as const },
];
</script>

<template>
  <div class="space-y-6">
    <PageHeader title="Policy">
      <template #description>
        What this installation counts as worth hearing: how many projects failing together is one problem, how long a
        failure may run with nobody acting, and how quiet somebody may go. Every number here applies to every
        project.
      </template>
      <template #actions>
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="loading"
          aria-label="Reload"
          @click="refresh"
        />
      </template>
    </PageHeader>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <UAlert
      v-else-if="!mayWrite && readOnlyReason"
      color="neutral"
      variant="soft"
      icon="i-lucide-lock"
      :description="readOnlyReason"
    />

    <template v-if="data">
      <PageSection
        title="Preset"
        description="Three, and the numbers underneath them. Choosing one replaces every number below with its own."
      >
        <div class="grid gap-3 sm:grid-cols-3">
          <button
            v-for="preset in data.presets"
            :key="preset.name"
            type="button"
            class="text-left rounded-md border px-4 py-3 disabled:opacity-60"
            :class="
              data.preset === preset.name && !data.modified ? 'border-primary bg-primary/5' : 'border-default hover:border-accented'
            "
            :disabled="!mayWrite || saving"
            @click="choose(preset.name)"
          >
            <p class="text-sm font-medium text-highlighted capitalize flex items-center gap-2">
              {{ preset.name }}
              <UIcon
                v-if="data.preset === preset.name && !data.modified"
                name="i-lucide-check"
                class="size-3.5 text-primary"
              />
            </p>
            <p class="text-xs text-muted mt-1">{{ preset.description }}</p>
          </button>
        </div>
        <p v-if="data.modified" class="text-xs text-warning mt-3 flex items-start gap-2">
          <UIcon name="i-lucide-pencil" class="size-3.5 shrink-0 mt-px" />
          <span>
            This installation is on <span class="capitalize">{{ data.preset }}</span> with numbers moved off it.
            Choosing a preset again puts them back.
          </span>
        </p>
      </PageSection>

      <PageSection
        title="The numbers"
        description="Every one of them is recorded on the findings it was evaluated against, so a condition from months ago can still be reproduced."
      >
        <div class="rounded-md border border-default divide-y divide-muted">
          <div
            v-for="field in fields"
            :key="field.key"
            class="px-4 py-3 flex items-start justify-between gap-4 flex-wrap"
          >
            <div class="min-w-0 flex-1">
              <p class="text-sm text-highlighted">{{ field.label }}</p>
              <p class="text-xs text-muted mt-0.5">{{ field.hint }}</p>
            </div>
            <div class="flex items-center gap-2 shrink-0">
              <UInput
                v-model.number="form[field.key]"
                type="number"
                size="sm"
                class="w-24"
                :disabled="!mayWrite || saving"
                :aria-label="field.label"
              />
              <span class="text-xs text-dimmed whitespace-nowrap">{{ field.unit }}</span>
            </div>
          </div>

          <div class="px-4 py-3 flex items-start justify-between gap-4 flex-wrap">
            <div class="min-w-0 flex-1">
              <p class="text-sm text-highlighted">Page for the top tier</p>
              <p class="text-xs text-muted mt-0.5">
                Off holds everything that would say “act now” down to “needs a fix”, for every project on this
                installation.
              </p>
            </div>
            <USwitch v-model="form.paging" :disabled="!mayWrite || saving" aria-label="Page for the top tier" />
          </div>
        </div>

        <div class="flex items-center justify-between gap-3 mt-3 flex-wrap">
          <p class="text-xs text-dimmed">
            A failure nobody acknowledges becomes a line on the compliance posture after
            <span class="font-mono">{{ untendedAfter }}</span
            >.
          </p>
          <UButton
            size="sm"
            :disabled="!mayWrite || !moved.length"
            :loading="saving"
            icon="i-lucide-save"
            @click="apply"
          >
            Save {{ moved.length ? `(${moved.length})` : "" }}
          </UButton>
        </div>
      </PageSection>

      <PageSection
        title="The confidence ladder"
        description="What the correlation threshold above is used for. A correlation is raised at the highest rung reachable and says which one that was — and it is never withheld for being unexplained."
      >
        <div class="rounded-md border border-default divide-y divide-muted">
          <div v-for="rung in ladder" :key="rung.confidence" class="px-4 py-3 flex items-start gap-3">
            <UIcon :name="confidenceIcon(rung.confidence)" class="size-4 shrink-0 mt-0.5 text-dimmed" />
            <div class="min-w-0">
              <p class="text-sm text-highlighted">{{ confidenceLabel(rung.confidence) }}</p>
              <p class="text-xs text-muted mt-0.5">{{ confidenceMeaning(rung.confidence) }}</p>
            </div>
          </div>
        </div>
      </PageSection>

      <p class="text-[11px] text-dimmed leading-relaxed">
        The catalogue itself is versioned code: which signals exist, what they compute and what each one asks of its
        reader are not settings, and cannot be. Only the clock is policy. That is why every finding carries the numbers
        it was evaluated against —
        <span class="font-mono break-all">{{ data.provenance }}</span> — so that two installations reporting the same
        catalogue version can still be told apart afterwards.
      </p>
    </template>
  </div>
</template>
