<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type ClaimRecoveries, type ClaimRecovery } from "../lib/api";
import { mayPromoteRecovery, promoteRefusal } from "../lib/claims";
import { timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";

// Recovering a claim's data to a moment in the past, where its provider can
// actually do it (#247).
//
// "Restore" is the wrong word and the screen has to say so. Nothing is
// rewound: recovering makes a **sibling** database holding the data as it was
// at the moment chosen, with an address of its own, while the application
// keeps reading the database it is reading. Deciding to use the sibling is a
// second act — **promote** — and it is the one with the blast radius, so it
// is the admin's and is confirmed by typing the claim's name.
//
// The window is the provider's own answer, not a setting: a claim whose
// provider cannot reach back at all offers nothing and says why, rather than
// showing a date picker over a span nobody has confirmed.

const props = defineProps<{
  claim: string;
  /** The caller's role on the claim's project, as it arrived on the
   * project's payload. It decides which of the two operations are offered. */
  role?: string;
  project: string;
}>();
const emit = defineEmits<{ changed: [] }>();

const toast = useToast();
const open = ref(false);

const data = ref<ClaimRecoveries | null>(null);
const loading = ref(false);
const failure = ref("");

const caller = computed(() => callerFor(props.role, props.project));
const mayRecover = computed(() => may("POST /api/v1/claims/{name}/recoveries", caller.value));
const mayPromote = computed(() => mayPromoteRecovery(caller.value));
const promoteRefused = computed(() => promoteRefusal(caller.value));

/** The moment to recover to, as the browser's local-time control spells it.
 * It is bounded by the window rather than validated after the fact, which is
 * the whole reason the window is read from the provider. */
const at = ref("");
const name = ref("");
const working = ref("");

/** A timestamp as `<input type="datetime-local">` takes it: local time, to
 * the minute, with no zone. */
function asLocalInput(iso: string | undefined): string {
  if (!iso) return "";
  const value = new Date(iso);
  if (Number.isNaN(value.getTime())) return "";
  const offset = value.getTime() - value.getTimezoneOffset() * 60_000;
  return new Date(offset).toISOString().slice(0, 16);
}

/** The same value read back out: the browser gives local time, the API takes
 * RFC 3339, and guessing between the two is a recovery to the wrong moment. */
function asTimestamp(local: string): string {
  const value = new Date(local);
  return Number.isNaN(value.getTime()) ? "" : value.toISOString();
}

const earliest = computed(() => asLocalInput(data.value?.window?.earliest));
const latest = computed(() => asLocalInput(data.value?.window?.latest));
const windowSpan = computed(() => {
  const window = data.value?.window;
  if (!window) return "";
  const hours = (new Date(window.latest).getTime() - new Date(window.earliest).getTime()) / 3_600_000;
  if (!Number.isFinite(hours) || hours <= 0) return "";
  if (hours < 48) return `${Math.floor(hours)} hours`;
  return `${Math.floor(hours / 24)} days`;
});

/** A moment inside the window, or the reason it is not one. The control is
 * bounded, so this catches a typed value and nothing else. */
const chosen = computed(() => {
  if (!at.value) return "Choose the moment to recover to.";
  const window = data.value?.window;
  const value = new Date(at.value).getTime();
  if (!window || Number.isNaN(value)) return "That is not a moment.";
  if (value < new Date(window.earliest).getTime()) return "That is further back than the provider can reach.";
  if (value > new Date(window.latest).getTime()) return "That is in the future.";
  return "";
});

async function load() {
  loading.value = true;
  failure.value = "";
  try {
    data.value = await api.claimRecoveries(props.claim);
    at.value = latest.value;
  } catch (err) {
    failure.value = err instanceof Error ? err.message : String(err);
  } finally {
    loading.value = false;
  }
}

watch(open, (isOpen) => {
  if (isOpen) void load();
});

async function recover() {
  if (working.value || chosen.value) return;
  working.value = "recover";
  try {
    data.value = await api.recoverClaim(props.claim, asTimestamp(at.value), name.value.trim() || undefined);
    toast.add({
      title: "Recovering",
      description: `A copy of ${props.claim} as it was then is being made. Nothing the application reads has changed.`,
      color: "success",
      icon: "i-lucide-history",
    });
    name.value = "";
    emit("changed");
  } catch (err) {
    toast.add({
      title: "Recovering failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    working.value = "";
  }
}

/** Promoting is confirmed by typing the claim's name, the same gate deleting
 * a project has: it replaces the database every environment reads, and a
 * click can be a slip. */
const toPromote = ref("");
const confirmation = ref("");
const promoteReady = computed(() => confirmation.value === props.claim);
watch(toPromote, () => {
  confirmation.value = "";
});

async function promote() {
  if (!toPromote.value || !promoteReady.value || working.value) return;
  working.value = "promote";
  try {
    data.value = await api.promoteClaimRecovery(props.claim, toPromote.value);
    toast.add({
      title: `${props.claim} is being cut over`,
      description:
        "Every environment reading this claim rolls onto the recovered copy. What it displaced is kept, not destroyed.",
      color: "success",
      icon: "i-lucide-git-branch",
    });
    toPromote.value = "";
    emit("changed");
  } catch (err) {
    toast.add({
      title: "Promoting failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    working.value = "";
  }
}

async function discard(recovery: string) {
  if (working.value) return;
  working.value = recovery;
  try {
    data.value = await api.discardClaimRecovery(props.claim, recovery);
    toast.add({
      title: `Recovery ${recovery} is being discarded`,
      description: "Its copy of the data goes with it.",
      color: "success",
      icon: "i-lucide-trash-2",
    });
    emit("changed");
  } catch (err) {
    toast.add({
      title: "Discarding failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    working.value = "";
  }
}

/** Whether this copy is the claim's binding, or on its way to being: the
 * status says what the operator has actually cut over, and `data.promoted`
 * says what was asked for a moment ago. Both hide the controls, because
 * neither is a copy anybody should be discarding. */
function isBound(recovery: ClaimRecovery): boolean {
  return Boolean(recovery.promoted) || data.value?.promoted === recovery.name;
}

function whenLocal(iso: string | undefined): string {
  if (!iso) return "—";
  const value = new Date(iso);
  return Number.isNaN(value.getTime()) ? "—" : value.toLocaleString();
}
</script>

<template>
  <UModal
    v-model:open="open"
    :title="`Recover ${props.claim}`"
    description="Recovering makes a copy of this data as it was at a moment, with an address of its own. Nothing the application reads changes until a copy is promoted."
  >
    <slot>
      <UButton icon="i-lucide-history" size="xs" color="neutral" variant="subtle">Recover</UButton>
    </slot>

    <template #body>
      <div class="space-y-6">
        <p v-if="loading" class="text-sm text-muted">Asking the provider what it can reach back to…</p>
        <p v-else-if="failure" class="text-sm text-error">{{ failure }}</p>

        <template v-else-if="data">
          <!-- The window, or the reason there is none. A claim whose provider
               cannot do this says which provider and why, rather than showing
               a control that would fail when used. -->
          <div v-if="!data.available" class="rounded-md border border-default px-4 py-3 space-y-1">
            <h2 class="text-sm font-medium text-highlighted">This claim cannot be recovered to a point in time</h2>
            <p class="text-xs text-toned">{{ data.reason }}</p>
          </div>

          <div v-else class="space-y-4">
            <div class="rounded-md border border-default px-4 py-3 space-y-1">
              <h2 class="text-sm font-medium text-highlighted">
                Its provider holds {{ windowSpan || "a window" }} of history
              </h2>
              <p class="text-xs text-toned">
                Anywhere from {{ whenLocal(data.window?.earliest) }} to {{ whenLocal(data.window?.latest) }}. Read from
                the provider {{ timeAgo(data.window?.observedAt) }}, not declared here — it moves when the plan does.
              </p>
            </div>

            <form v-if="mayRecover" class="space-y-3" @submit.prevent="recover">
              <div class="grid gap-3 sm:grid-cols-2">
                <UFormField label="Recover to" :help="chosen || 'Inside the window above.'" required>
                  <UInput v-model="at" type="datetime-local" :min="earliest" :max="latest" class="w-full" />
                </UFormField>
                <UFormField label="Call it" help="Optional. Empty names it after the moment.">
                  <UInput v-model="name" placeholder="before-the-migration" class="w-full font-mono" />
                </UFormField>
              </div>
              <div class="flex justify-end">
                <UButton
                  type="submit"
                  size="sm"
                  icon="i-lucide-history"
                  :disabled="Boolean(chosen)"
                  :loading="working === 'recover'"
                >
                  Recover to a copy
                </UButton>
              </div>
            </form>
          </div>

          <!-- The copies that exist, with what each holds and what may be
               done with it. -->
          <div class="space-y-2">
            <h2 class="text-sm font-medium text-highlighted">Recovered copies</h2>
            <div class="rounded-md border border-default overflow-x-auto">
              <table class="w-full min-w-[36rem] text-sm">
                <tbody>
                  <tr v-if="!data.recoveries.length">
                    <td class="px-3 py-8 text-center text-muted">
                      No copies. Recovering makes one; it costs the original nothing.
                    </td>
                  </tr>
                  <tr
                    v-for="recovery in data.recoveries"
                    :key="recovery.name"
                    class="border-b border-muted last:border-0"
                  >
                    <td class="px-3 py-2 font-mono text-xs text-highlighted">{{ recovery.name }}</td>
                    <td class="px-3 py-2 text-xs text-toned whitespace-nowrap">
                      as at {{ whenLocal(recovery.at) }}
                    </td>
                    <td class="px-3 py-2 text-xs">
                      <UBadge v-if="isBound(recovery)" color="warning" variant="subtle" size="sm">
                        {{ recovery.promoted ? "bound now" : "cutting over" }}
                      </UBadge>
                      <UBadge v-else color="neutral" variant="subtle" size="sm">
                        {{ recovery.phase || "Pending" }}
                      </UBadge>
                    </td>
                    <td class="px-3 py-2 text-xs text-dimmed whitespace-nowrap">
                      {{ recovery.dataClass || "unclassified" }} ·
                      {{ recovery.provenance || "undeclared" }}
                    </td>
                    <td class="px-3 py-2 text-right whitespace-nowrap">
                      <UButton
                        v-if="!isBound(recovery) && mayPromote"
                        size="xs"
                        color="warning"
                        variant="subtle"
                        :disabled="recovery.phase !== 'Ready'"
                        @click="toPromote = recovery.name"
                      >
                        Promote
                      </UButton>
                      <span v-else-if="!isBound(recovery)" class="text-xs text-muted" :title="promoteRefused">
                        admin only
                      </span>
                      <UButton
                        v-if="!isBound(recovery)"
                        class="ml-1"
                        size="xs"
                        color="neutral"
                        variant="subtle"
                        icon="i-lucide-trash-2"
                        :loading="working === recovery.name"
                        @click="discard(recovery.name)"
                      >
                        Discard
                      </UButton>
                    </td>
                  </tr>
                  <!-- The provider's own words for a copy it refused, next to
                       the copy rather than in a toast that has gone. -->
                  <tr
                    v-for="recovery in data.recoveries.filter((r) => r.phase === 'Failed')"
                    :key="`${recovery.name}-why`"
                    class="border-b border-muted last:border-0"
                  >
                    <td colspan="5" class="px-3 py-2 text-xs text-error">
                      <span class="font-mono">{{ recovery.name }}</span> — {{ recovery.message }}
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>

          <!-- What promoting has displaced. Kept, always: destroying data is
               opted into, and a promote is exactly the moment somebody might
               have chosen the wrong timestamp. -->
          <div v-if="data.retained?.length" class="space-y-2">
            <h2 class="text-sm font-medium text-highlighted">Kept, no longer bound</h2>
            <ul class="text-xs text-toned space-y-1">
              <li v-for="(retained, index) in data.retained" :key="index">
                {{ retained.recovery || "the database this claim was provisioned with" }} — displaced by
                <span class="font-mono">{{ retained.displacedBy }}</span> {{ timeAgo(retained.at) }}. It still holds its
                data; nothing here removes it.
              </li>
            </ul>
          </div>
        </template>
      </div>
    </template>
  </UModal>

  <!-- Promoting is the destructive half: it replaces the database every
       environment of this project reads. Same gate as deleting a project —
       typing the name — because a click can be a slip. -->
  <UModal
    :open="toPromote !== ''"
    :title="`Promote ${toPromote} over ${props.claim}?`"
    :description="`Every environment reading ${props.claim} rolls onto this copy, and does so one deploy at a time rather than all at once — for that window, some of them are still writing to the database being replaced. What is displaced is kept, not destroyed.`"
    @update:open="(isOpen: boolean) => { if (!isOpen) toPromote = ''; }"
  >
    <template #body>
      <UInput v-model="confirmation" :placeholder="`Type ${props.claim} to confirm`" class="w-full font-mono" />
    </template>
    <template #footer>
      <div class="flex justify-end gap-2 w-full">
        <UButton color="neutral" variant="subtle" @click="toPromote = ''">Cancel</UButton>
        <UButton
          color="warning"
          icon="i-lucide-git-branch"
          :disabled="!promoteReady"
          :loading="working === 'promote'"
          @click="promote"
        >
          Promote and cut over
        </UButton>
      </div>
    </template>
  </UModal>
</template>
