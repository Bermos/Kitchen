<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type Condition, type Operator, type Settings } from "../lib/api";
import { callerFor } from "../lib/me";
import {
  alreadyListed,
  describeOperator,
  isLastOperator,
  operatorList,
  operatorsCondition,
  operatorsNote,
  operatorsState,
  wasSeeded,
  withOperator,
  withoutOperator,
  OPERATORS_STATE_NOTE,
} from "../lib/operators";
import { may, refusal } from "../lib/policy";

// Who holds the operator role — the real list, and the write for it.
//
// It is `spec.access.operators` on the Kitchen singleton, which is what every
// `operator` requirement in the policy table is resolved against, and it is
// served here because this screen already carries the base domain, the issuer
// and the gateway address. Before this panel the dashboard showed the
// reconciler's `OperatorsConfigured` condition instead — which names at most
// five accounts and counts the rest, so on any installation with more than
// five the only way to find out who held the platform was `kubectl`.
//
// **The reading that matters is "seeded, not chosen".** An installation that
// had never named an operator has the list seeded on upgrade from every
// account the identity provider holds, because before enforcement every one of
// those accounts really could call every route. That list is nobody's
// decision, and narrowing it should be one click from here. The condition is
// still shown next to the list, because the list says who and the condition
// says how they got there.
//
// Two refusals are shown where the control that caused them is, rather than
// swallowed into a toast:
//
//   - **Emptying the list** is a 409, and its sentence is the whole reason:
//     a platform with no operator has nobody left who can appoint one, and the
//     only way back is editing the Kitchen object with kubectl.
//   - **An address the identity provider has never seen** is a 404 about the
//     person, because an address is resolved to the issuer's `sub` before
//     anything is written — they have to sign in once before they can be given
//     the role.

const props = defineProps<{ settings: Settings }>();
const emit = defineEmits<{ saved: [] }>();

const toast = useToast();

// The operator list is written through PATCH /settings, so that is the route
// the control is keyed to. Everybody who can see this screen is an operator —
// GET /settings is operator-only — but the question a control asks is still
// asked of the table rather than assumed from the screen it is on.
const caller = computed(() => callerFor());
const mayWrite = computed(() => may("PATCH /api/v1/settings", caller.value));
const readOnlyReason = computed(() => refusal("PATCH /api/v1/settings", caller.value));

const operators = computed<Operator[]>(() => operatorList(props.settings.operators));
const state = computed(() => operatorsState(props.settings.operators));
const stateNote = computed(() => OPERATORS_STATE_NOTE[state.value]);
const condition = computed<Condition | undefined>(() => operatorsCondition(props.settings.conditions));
const conditionNote = computed(() => operatorsNote(condition.value));
const seeded = computed(() => wasSeeded(condition.value));

// One line for whatever the last write ran into, in the API's own words.
const writeError = ref("");

const email = ref("");
const adding = ref(false);
async function add() {
  const address = email.value.trim();
  if (!address || adding.value) return;
  if (alreadyListed(operators.value, address)) {
    writeError.value = `${address} is already an operator.`;
    return;
  }
  adding.value = true;
  writeError.value = "";
  try {
    await api.updateSettings({ operators: withOperator(operators.value, address) });
    toast.add({ title: `${address} is an operator`, color: "success", icon: "i-lucide-shield-check" });
    email.value = "";
    emit("saved");
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
  } finally {
    adding.value = false;
  }
}

const removing = ref<Operator | null>(null);
const removed = ref(false);
async function remove() {
  const operator = removing.value;
  if (!operator || removed.value) return;
  removed.value = true;
  writeError.value = "";
  try {
    await api.updateSettings({ operators: withoutOperator(operators.value, operator.subject) });
    toast.add({
      title: `${describeOperator(operator)} is no longer an operator`,
      color: "success",
      icon: "i-lucide-shield-off",
    });
    removing.value = null;
    emit("saved");
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
    removing.value = null;
  } finally {
    removed.value = false;
  }
}

// Removing the last one is refused by the API and the refusal is rendered —
// but it is also worth saying before the click, because the reason it is
// refused is the reason nobody wants to try it.
const removalBlurb = computed(() => {
  const operator = removing.value;
  if (!operator) return "";
  if (isLastOperator(operators.value)) {
    return "This is the only operator on the platform. The API refuses to empty the list — a platform with no operator has nobody left who can appoint one — so name whoever is to stay first.";
  }
  return `They keep any project role they hold in their own right, and lose the platform surface: the platform screens, the connections, the settings, and admin on every project they are not listed on.`;
});
</script>

<template>
  <div class="rounded-md border border-default px-5 py-4 space-y-4">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-sm font-medium text-highlighted">Who holds the operator role</h2>
        <p class="text-xs text-muted mt-1">{{ stateNote }}</p>
      </div>
    </div>

    <UAlert
      v-if="seeded"
      color="warning"
      variant="soft"
      icon="i-lucide-list-checks"
      title="This list was seeded, not chosen"
      description="It was filled from the accounts that existed when the platform started enforcing roles — every one of which could already call every route. Narrowing it to the people who should hold the platform is a deliberate edit, and this is where it is made."
    />

    <UAlert
      v-if="writeError"
      color="warning"
      variant="soft"
      icon="i-lucide-info"
      :title="writeError"
      close
      @update:open="writeError = ''"
    />

    <div v-if="state === 'named'" class="rounded-md border border-default overflow-x-auto">
      <table class="w-full min-w-[28rem] text-sm">
        <tbody>
          <tr v-for="operator in operators" :key="operator.subject" class="border-b border-muted last:border-0">
            <td class="px-4 py-3">
              <p class="text-highlighted">{{ describeOperator(operator) }}</p>
              <p v-if="operator.email" class="text-xs text-dimmed font-mono truncate max-w-xs" :title="operator.subject">
                {{ operator.subject }}
              </p>
            </td>
            <td class="px-4 py-3 text-right whitespace-nowrap">
              <UButton
                v-if="mayWrite"
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-user-minus"
                :aria-label="`Remove ${describeOperator(operator)}`"
                @click="removing = operator"
              />
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <p v-else-if="state === 'nobody'" class="text-sm text-toned">
      Nobody. The platform surface is closed to every account until somebody is named here.
    </p>

    <template v-if="condition">
      <p class="text-xs text-muted">
        <span class="font-mono">{{ condition.reason || condition.type }}</span
        >: {{ condition.message || "—" }}
      </p>
      <p v-if="conditionNote" class="text-xs text-dimmed">{{ conditionNote }}</p>
    </template>

    <form v-if="mayWrite && state !== 'unserved'" class="flex items-end gap-2 flex-wrap" @submit.prevent="add">
      <UFormField
        label="Add an operator"
        help="The address they sign in with. It is resolved at the identity provider before anything is written, so they have to have signed in once."
      >
        <UInput v-model="email" type="email" placeholder="anna@example.com" class="w-64" />
      </UFormField>
      <UButton type="submit" :disabled="!email.trim()" :loading="adding" icon="i-lucide-shield-plus">Add</UButton>
    </form>
    <p v-else-if="readOnlyReason" class="text-xs text-muted">{{ readOnlyReason }}.</p>

    <p class="text-xs text-dimmed">
      An operator holds admin on every project, present and future, and everything under Platform. The list is
      <span class="font-mono">spec.access.operators</span> on the <span class="font-mono">Kitchen</span> singleton;
      everything else on it — the base domain, the issuer, the ingress — shapes URLs the platform has already handed
      out, and stays a deliberate kubectl operation.
    </p>

    <UModal
      :open="removing !== null"
      :title="`Remove ${removing ? describeOperator(removing) : ''} from the operators?`"
      :description="removalBlurb"
      @update:open="(open: boolean) => { if (!open) removing = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="removing = null">Cancel</UButton>
          <UButton color="error" :loading="removed" icon="i-lucide-shield-off" @click="remove">Remove operator</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
