<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, CRITICALITIES, type Environment } from "../lib/api";
import { callerFor, isOperator, me } from "../lib/me";
import { may } from "../lib/policy";
import {
  bundleDigestProblem,
  formatParameters,
  mayOwn,
  parseOwners,
  parseParameters,
} from "../lib/requirements";
import { useAsync } from "../lib/useAsync";

// The bar this environment sets, and how the deployed release measures up.
//
// Two different people read this panel. The deploying team reads what they
// will be judged against — the bundle, its parameters, and per release the
// evidence the artifact carries against it. The environment's owners (and
// operators) additionally get the edit, which is the one control on the whole
// screen that is not the project role's: ownership is written on the
// environment, the API enforces it in the handler, and the panel checks the
// same list only to decide whether to draw the button.

const props = defineProps<{ environment: Environment; role?: string }>();
const emit = defineEmits<{ changed: [] }>();
const toast = useToast();

const caller = computed(() => callerFor(props.role, props.environment.project));
const editable = computed(
  () =>
    may("PATCH /api/v1/environments/{name}/requirements", caller.value) &&
    mayOwn(props.environment.owners, me.value, isOperator.value),
);

// How the deployed release measures up — asked only when something is
// deployed, and re-asked when the release moves.
const eligibility = useAsync(
  () => api.environmentEligibility(props.environment.name),
  { immediate: !!props.environment.release },
);
watch(
  () => [props.environment.name, props.environment.release],
  () => {
    if (props.environment.release) void eligibility.refresh();
  },
);

const verdict = computed(() => {
  const answer = eligibility.data.value;
  if (!answer) return null;
  if (answer.eligible === true) return { label: "Eligible", color: "success" as const };
  if (answer.eligible === false) return { label: "Not eligible", color: "error" as const };
  return { label: "Not evaluated yet", color: "neutral" as const };
});

// The edit: three fields, each holding its whole list, because that is
// exactly what the PATCH replaces. An empty digest removes the bar.
const editing = ref(false);
const digest = ref("");
const parameters = ref("");
const owners = ref("");
const criticality = ref("");
const rto = ref("");
const rpo = ref("");
const saving = ref(false);

const criticalityOptions = [
  { label: "undesignated", value: "" },
  ...CRITICALITIES.map((value) => ({ label: value, value: value as string })),
];

function openEditor() {
  digest.value = props.environment.requirements?.bundleDigest ?? "";
  parameters.value = formatParameters(props.environment.requirements?.parameters);
  owners.value = (props.environment.owners ?? []).join("\n");
  criticality.value = props.environment.criticality ?? "";
  rto.value = props.environment.rto ?? "";
  rpo.value = props.environment.rpo ?? "";
  editing.value = true;
}

/** The environment's own designation as one line. Absent is a word, not a
 *  blank: this environment declaring nothing is a different state from
 *  nothing applying to it — production reads its project's designation, and
 *  the criticality map on the compliance screen answers with that resolved. */
const declared = computed(() => {
  const parts: string[] = [];
  if (props.environment.criticality) parts.push(props.environment.criticality);
  if (props.environment.rto) parts.push(`RTO ${props.environment.rto}`);
  if (props.environment.rpo) parts.push(`RPO ${props.environment.rpo}`);
  return parts.join(" · ");
});

const digestProblem = computed(() => bundleDigestProblem(digest.value));
const parameterProblem = computed(() => parseParameters(parameters.value).problem);

async function save() {
  if (digestProblem.value || parameterProblem.value) return;
  saving.value = true;
  try {
    const trimmed = digest.value.trim();
    await api.patchEnvironmentRequirements(props.environment.name, {
      bundleDigest: trimmed,
      ...(trimmed !== "" ? { parameters: parseParameters(parameters.value).parameters } : {}),
      owners: parseOwners(owners.value),
      criticality: criticality.value,
      rto: rto.value.trim(),
      rpo: rpo.value.trim(),
    });
    toast.add({ title: "Requirements updated", color: "success", icon: "i-lucide-shield-check" });
    editing.value = false;
    emit("changed");
    if (props.environment.release) void eligibility.refresh();
  } catch (err) {
    toast.add({
      title: "Changing the requirements failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <div>
    <div class="flex items-center justify-between gap-3 mb-2">
      <h2 class="text-sm font-medium text-highlighted">Requirements</h2>
      <UButton v-if="editable" color="neutral" variant="subtle" size="xs" icon="i-lucide-pencil" @click="openEditor">
        Edit
      </UButton>
    </div>
    <p class="text-xs text-muted mb-3">
      What an artifact must bring to land here, declared by this environment's owners — not by the project deploying
      into it. Evaluation reads the attestations on the artifact, never a live check.
    </p>

    <div class="rounded-md border border-default overflow-hidden">
      <div class="bg-muted px-5 py-4 grid gap-6 sm:grid-cols-3 text-sm">
        <div class="min-w-0">
          <p class="text-xs text-muted mb-1">Owners</p>
          <div v-if="environment.owners?.length" class="flex flex-wrap gap-1">
            <UBadge v-for="owner in environment.owners" :key="owner" color="neutral" variant="subtle" size="sm">
              {{ owner }}
            </UBadge>
          </div>
          <p v-else class="text-xs text-dimmed">none — only platform operators may change the bar</p>
        </div>
        <div class="min-w-0">
          <p class="text-xs text-muted mb-1">Policy bundle</p>
          <p
            v-if="environment.requirements"
            class="font-mono text-xs text-highlighted truncate"
            :title="environment.requirements.bundleDigest"
          >
            {{ environment.requirements.bundleDigest }}
          </p>
          <p v-else class="text-xs text-dimmed">no requirements — every release is eligible</p>
        </div>
        <div class="min-w-0">
          <p class="text-xs text-muted mb-1">Parameters</p>
          <div v-if="Object.keys(environment.requirements?.parameters ?? {}).length" class="space-y-0.5">
            <p
              v-for="(value, name) in environment.requirements?.parameters"
              :key="name"
              class="font-mono text-xs text-toned"
            >
              {{ name }}={{ value }}
            </p>
          </div>
          <p v-else class="text-xs text-dimmed">—</p>
        </div>
      </div>

      <!-- The environment's own continuity designation, beside the bar
           because it is the same kind of declaration by the same people. -->
      <div class="border-t border-default px-5 py-4">
        <div class="flex items-baseline gap-3 flex-wrap">
          <p class="text-xs text-muted">Continuity</p>
          <p v-if="declared" class="text-xs font-mono text-toned">{{ declared }}</p>
          <p v-else class="text-xs text-dimmed">
            this environment declares nothing — a production environment then reads its project's
            designation, a preview reads none
          </p>
        </div>
        <p class="text-xs text-dimmed mt-1 max-w-3xl">
          Kitchen does not decide what is critical and does not set the tolerances — the institution does.
          Setting an RTO here changes when an outage of this environment wakes somebody; designating it
          critical raises every warning about it to a critical finding. Neither refuses a deployment.
        </p>
      </div>

      <!-- The deployed release against the bar. -->
      <div v-if="environment.release" class="border-t border-default px-5 py-4">
        <UAlert
          v-if="eligibility.error.value"
          color="error"
          variant="soft"
          icon="i-lucide-triangle-alert"
          :title="eligibility.error.value"
        />
        <template v-else-if="eligibility.data.value">
          <div class="flex items-center gap-3 flex-wrap text-sm">
            <span class="font-mono text-xs text-toned">{{ eligibility.data.value.release }}</span>
            <UBadge v-if="verdict" :color="verdict.color" variant="soft" size="sm">{{ verdict.label }}</UBadge>
            <span v-if="eligibility.data.value.message" class="text-xs text-muted">
              {{ eligibility.data.value.message }}
            </span>
          </div>
          <ul v-if="eligibility.data.value.unmetRules.length" class="mt-2 space-y-1">
            <li
              v-for="rule in eligibility.data.value.unmetRules"
              :key="rule"
              class="text-xs text-error font-mono flex items-center gap-2"
            >
              <UIcon name="i-lucide-x" class="size-3.5" />{{ rule }}
            </li>
          </ul>
          <div v-if="eligibility.data.value.evidence.length" class="mt-3 overflow-x-auto">
            <table class="w-full min-w-[28rem] text-xs">
              <thead>
                <tr class="text-left text-muted border-b border-default">
                  <th class="py-1.5 pr-4 font-medium">Evidence</th>
                  <th class="py-1.5 pr-4 font-medium">Source</th>
                  <th class="py-1.5 font-medium">Verified</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="evidence in eligibility.data.value.evidence"
                  :key="evidence.predicateType + (evidence.source ?? '')"
                  class="border-b border-muted last:border-0"
                >
                  <td class="py-1.5 pr-4 font-mono text-toned truncate max-w-md" :title="evidence.predicateType">
                    {{ evidence.predicateType }}
                  </td>
                  <td class="py-1.5 pr-4 text-muted">{{ evidence.source || "—" }}</td>
                  <td class="py-1.5">
                    <UIcon v-if="evidence.verified" name="i-lucide-shield-check" class="size-4 text-success" />
                    <span v-else class="text-dimmed">no</span>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="mt-2 text-xs text-dimmed">The artifact carries no evidence.</p>
        </template>
        <p v-else class="text-xs text-muted">Loading…</p>
      </div>
    </div>

    <UModal
      :open="editing"
      :title="`Requirements of ${environment.name}`"
      description="The digest pins the policy bundle a release is judged against; an empty digest removes the requirements. Parameters and owners each replace their whole list."
      @update:open="(open: boolean) => { editing = open; }"
    >
      <template #body>
        <div class="space-y-4">
          <UFormField label="Bundle digest" :error="digestProblem">
            <UInput v-model="digest" class="w-full font-mono" placeholder="sha256:…" />
          </UFormField>
          <UFormField label="Parameters" :error="parameterProblem" help="One name=value per line.">
            <UTextarea v-model="parameters" class="w-full font-mono" :rows="3" placeholder="maxSeverity=high" />
          </UFormField>
          <UFormField label="Owners" help="One per line: an issuer subject, or a verified email address. Empty leaves changes to platform operators alone.">
            <UTextarea v-model="owners" class="w-full font-mono" :rows="3" placeholder="risk-officer@example.com" />
          </UFormField>
          <div class="grid gap-3 sm:grid-cols-3">
            <UFormField label="Criticality" help="The institution's designation, not Kitchen's.">
              <USelect v-model="criticality" :items="criticalityOptions" class="w-full" />
            </UFormField>
            <UFormField label="RTO" help="4h, 30m, 1h30m.">
              <UInput v-model="rto" class="w-full font-mono" placeholder="unset" />
            </UFormField>
            <UFormField label="RPO" help="Same spelling.">
              <UInput v-model="rpo" class="w-full font-mono" placeholder="unset" />
            </UFormField>
          </div>
          <p class="text-xs text-dimmed">
            Kitchen does not decide what is critical and does not set the tolerances. A preview of a critical
            project is not a critical function, and nothing here is capped by the project's designation —
            this environment is designated on its own terms.
          </p>
        </div>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="editing = false">Cancel</UButton>
          <UButton
            color="primary"
            :loading="saving"
            :disabled="!!digestProblem || !!parameterProblem"
            icon="i-lucide-shield-check"
            @click="save"
          >
            Save
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
