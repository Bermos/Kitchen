<script setup lang="ts">
import { computed, ref, watch } from "vue";
import {
  api,
  type PlatformRetention,
  type PlatformRetentionPatch,
} from "../lib/api";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";
import { useAsync } from "../lib/useAsync";
import StatusDot from "./StatusDot.vue";

// How long the platform keeps each class of what it holds, and how far back
// each class actually goes.
//
// The table is deliberately two things at once: the left half is the rule —
// the number, and whether anybody chose it or it is inherited — and the right
// half is the measurement the retention sweep took. The measurement is the
// point. "We keep container logs for thirty days" is a setting; "nothing older
// than 2026-07-25 is there, measured yesterday" is the answer to the question
// an auditor is actually asking, and it is the one the screen leads with.
//
// The audit floor is shown as a floor rather than enforced by the input's
// `min`: a number under it is a legitimate thing to ask for, and what happens
// then is that the API refuses it and names the override — which is a sentence
// somebody should read, not a control that quietly will not move.

const toast = useToast();
const { data, error, loading, refresh } = useAsync(() =>
  api.platformRetention(),
);

const caller = computed(() => callerFor());
const mayWrite = computed(() =>
  may("PATCH /api/v1/platform/retention", caller.value),
);
const readOnlyReason = computed(() =>
  refusal("PATCH /api/v1/platform/retention", caller.value),
);

// The form is a copy of the served days, keyed by class, so that only what
// moved is sent — the route leaves an absent class alone, and sending all nine
// back would make every save a change to every class in the audit log.
const days = ref<Record<string, number>>({});
watch(data, (value: PlatformRetention | null) => {
  if (!value) return;
  const next: Record<string, number> = {};
  for (const entry of value.classes) next[entry.class] = entry.days;
  days.value = next;
});

const moved = computed(() => {
  const value = data.value;
  if (!value) return [] as string[];
  return value.classes
    .filter((entry) => days.value[entry.class] !== entry.days)
    .map((entry) => entry.class);
});

// The override the form would send, and only while the audit class is being
// moved under the floor — an override typed for a retention that clears the
// floor would be a decision recorded about nothing.
const overrideReason = ref("");
const overrideApprovedBy = ref("");
const needsOverride = computed(() => {
  const value = data.value;
  if (!value) return false;
  const audit = days.value["audit"];
  return (
    audit !== undefined &&
    audit < value.auditFloorDays &&
    !value.auditFloorOverride
  );
});

const saving = ref(false);
async function save() {
  const value = data.value;
  if (!value) return;
  const body: PlatformRetentionPatch = {};
  for (const name of moved.value) {
    (body as Record<string, number>)[name] = days.value[name];
  }
  if (needsOverride.value) {
    body.auditFloorOverride = {
      reason: overrideReason.value,
      approvedBy: overrideApprovedBy.value,
    };
  }

  saving.value = true;
  try {
    await api.updatePlatformRetention(body);
    toast.add({
      title: "Retention saved",
      color: "success",
      icon: "i-lucide-check",
    });
    overrideReason.value = "";
    overrideApprovedBy.value = "";
    await refresh();
  } catch (err) {
    // The floor's refusal is a sentence worth reading in full — it names the
    // field, the number and the way past it — so it is shown as the
    // description rather than trimmed to a title.
    toast.add({
      title: "Retention not changed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    saving.value = false;
  }
}

function measured(entry: { oldest?: string }): string {
  if (!entry.oldest) return "—";
  return new Date(entry.oldest).toLocaleDateString();
}

const sweptAt = computed(() =>
  data.value?.lastSweep ? new Date(data.value.lastSweep).toLocaleString() : "",
);
</script>

<template>
  <div class="rounded-md border border-default px-5 py-4 space-y-4">
    <div>
      <h2 class="text-sm font-medium text-highlighted">Retention</h2>
      <p class="text-xs text-muted mt-1">
        How long each class of what the platform keeps is kept, and how far back
        each one actually goes. The
        <span class="font-mono">oldest</span> column is the claim retention
        makes: nothing of that class is older than this.
        <template v-if="sweptAt"> Last measured {{ sweptAt }}.</template>
      </p>
    </div>

    <UAlert
      v-if="error"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="error"
    />
    <UAlert
      v-else-if="data?.message"
      color="warning"
      variant="soft"
      icon="i-lucide-info"
      :description="data.message"
    />

    <template v-if="data">
      <div class="rounded-md border border-default bg-muted overflow-x-auto">
        <table class="w-full min-w-[42rem] text-sm">
          <thead>
            <tr class="text-left text-xs text-muted border-b border-default">
              <th class="px-3 py-2 font-medium">Class</th>
              <th class="px-3 py-2 font-medium">Kept (days)</th>
              <th class="px-3 py-2 font-medium">Set by</th>
              <th class="px-3 py-2 font-medium">Oldest</th>
              <th class="px-3 py-2 font-medium">Rows</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="entry in data.classes"
              :key="entry.class"
              class="border-b border-muted last:border-0"
            >
              <td class="px-3 py-2">
                <span class="inline-flex items-center gap-2">
                  <StatusDot :tone="entry.enforced ? 'success' : 'neutral'" />
                  <span>
                    <span class="text-highlighted">{{ entry.label }}</span>
                    <span class="block text-xs text-muted">{{
                      entry.description
                    }}</span>
                  </span>
                </span>
              </td>
              <td class="px-3 py-2">
                <UInputNumber
                  v-model="days[entry.class]"
                  :min="1"
                  :disabled="!mayWrite"
                  class="w-28"
                />
              </td>
              <td class="px-3 py-2 font-mono text-xs text-toned">
                {{ entry.source === "retention" ? "set here" : entry.source }}
              </td>
              <td
                class="px-3 py-2 font-mono text-xs"
                :class="entry.oldest ? 'text-toned' : 'text-muted'"
              >
                {{ measured(entry) }}
              </td>
              <td class="px-3 py-2 font-mono text-xs text-toned">
                {{ entry.rows ?? "—" }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <UAlert
        v-if="data.auditFloorOverridden && data.auditFloorOverride"
        color="warning"
        variant="soft"
        icon="i-lucide-shield-alert"
        :title="`Audit records are kept for less than the ${data.auditFloorDays}-day floor`"
        :description="`${data.auditFloorOverride.approvedBy}: ${data.auditFloorOverride.reason}`"
      />

      <div
        v-if="needsOverride"
        class="rounded-md border border-warning/40 bg-warning/5 px-4 py-3 space-y-3"
      >
        <p class="text-xs text-toned">
          Audit records are the evidence an incident is reconstructed from, and
          an incident reporting duty runs from when an institution became aware
          — which can be long after the transition that caused it. Keeping less
          than
          {{ data.auditFloorDays }} days needs a written override, and using it
          is itself an audit record.
        </p>
        <UFormField
          label="Why"
          help="Read by whoever asks why the log does not go back far enough."
        >
          <UTextarea v-model="overrideReason" :rows="2" class="w-full" />
        </UFormField>
        <UFormField
          label="Approved by"
          help="Whoever decided it, as a name or an address."
        >
          <UInput v-model="overrideApprovedBy" class="w-full max-w-72" />
        </UFormField>
      </div>

      <div class="flex items-center justify-end gap-3">
        <p v-if="!mayWrite" class="text-xs text-muted">{{ readOnlyReason }}</p>
        <UButton
          :disabled="!mayWrite || !moved.length"
          :loading="saving"
          icon="i-lucide-save"
          @click="save"
          >Save retention</UButton
        >
      </div>
    </template>
    <div v-else-if="loading" class="py-8 text-center text-muted text-sm">
      Loading…
    </div>
  </div>
</template>
