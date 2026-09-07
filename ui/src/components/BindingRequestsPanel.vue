<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type BindingRequest } from "../lib/api";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";
import { timeAgo } from "../lib/format";

// Who has asked to bind this project's offerings, and this project's answer
// (#495).
//
// It is on the *provider's* project screen because that is where the people
// who can answer already look, and because the answer is theirs alone: an
// offering whose visibility is `request` admits nobody until an admin here
// says so. The consumer's side of the same exchange is its own claim, which
// says what it is waiting for and, if it was refused, why.
//
// **Approving provisions nothing by itself.** It writes the grant, and the
// claim's own reconcile then resolves the address exactly as it would have
// on an open offering — one bind path, not two. Refusing takes the address
// back the same way, which is why *withdraw* and *refuse* are one button
// here: they are one fact — this project is not admitted — and the reason
// says which of them it was.

const props = defineProps<{
  project: string;
  role?: string;
  requests: BindingRequest[];
}>();
const emit = defineEmits<{ decided: [] }>();

const toast = useToast();

const caller = computed(() => callerFor(props.role, props.project));
const mayDecide = computed(() =>
  may("PATCH /api/v1/projects/{name}/requests/{claim}", caller.value),
);
const readOnlyReason = computed(() =>
  refusal("PATCH /api/v1/projects/{name}/requests/{claim}", caller.value),
);

/** Waiting for somebody here, which is what the pane is for. */
const waiting = computed(() =>
  props.requests.filter((request) => request.state === "requested"),
);
/** Everything already answered, and the consumers an open offering let in
 * without anybody being asked. It stays on the screen because this is also
 * where a grant is withdrawn: one nobody can find again is one nobody can
 * take back. */
const settled = computed(() =>
  props.requests.filter((request) => request.state !== "requested"),
);

function label(request: BindingRequest): string {
  if (request.state === "approved") {
    return request.open ? "admitted — the offering was open" : "admitted";
  }
  if (request.state === "denied") return "not admitted";
  return "waiting";
}

/** An open offering admits every project by its own terms, so there is
 * nothing here for a decision to settle — and the API says so rather than
 * writing a grant that changes nothing. Saying it on the row is what keeps
 * the button from being one whose only answer is an error. */
function decidable(request: BindingRequest): boolean {
  return request.visibility === "request";
}

const deciding = ref("");
const denying = ref<BindingRequest | null>(null);
const denyReason = ref("");
const writeError = ref("");

async function decide(request: BindingRequest, decision: string, reason?: string) {
  if (deciding.value) return;
  deciding.value = request.claim;
  writeError.value = "";
  try {
    await api.decideBindingRequest(props.project, request.claim, {
      decision,
      ...(reason ? { reason } : {}),
    });
    denying.value = null;
    denyReason.value = "";
    toast.add({
      title:
        decision === "approved"
          ? `${request.project} may bind ${request.offering}`
          : `${request.project} is not admitted to ${request.offering}`,
      description:
        decision === "approved"
          ? "The address reaches its workloads on their next deploy."
          : "The address is taken back, and its environments roll without it.",
      color: decision === "approved" ? "success" : "neutral",
      icon: decision === "approved" ? "i-lucide-check" : "i-lucide-x",
    });
    emit("decided");
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
  } finally {
    deciding.value = "";
  }
}
</script>

<template>
  <div class="space-y-4">
    <div>
      <h2 class="text-sm font-medium text-highlighted">Binding requests</h2>
      <p class="text-xs text-muted mt-1">
        Projects asking to reach one of this project's offerings. An offering that admits consumers by request binds
        nobody until this project answers; nothing is provisioned while a request waits.
      </p>
    </div>

    <UAlert
      v-if="writeError"
      color="warning"
      variant="soft"
      icon="i-lucide-info"
      :title="writeError"
      close
      @update:open="writeError = ''"
    />

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full min-w-[44rem] text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default">
            <th class="px-3 py-2 font-medium">Project</th>
            <th class="px-3 py-2 font-medium">Offering</th>
            <th class="px-3 py-2 font-medium">Asked</th>
            <th class="px-3 py-2 font-medium">State</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!requests.length">
            <td colspan="5" class="px-3 py-8 text-center text-muted">
              Nobody has asked to bind anything here.
            </td>
          </tr>
          <tr
            v-for="request in [...waiting, ...settled]"
            :key="request.claim"
            class="border-b border-muted last:border-0"
          >
            <td class="px-3 py-2">
              <p class="text-highlighted font-mono">{{ request.project }}</p>
              <p class="text-xs text-dimmed mt-1">
                claim <span class="font-mono">{{ request.claim }}</span>
              </p>
            </td>
            <td class="px-3 py-2 font-mono text-muted">{{ request.offering }}</td>
            <td class="px-3 py-2 text-xs text-muted">
              <template v-if="request.requestedAt">
                {{ timeAgo(request.requestedAt) }}
                <span v-if="request.requestedBy" class="text-dimmed">by {{ request.requestedBy }}</span>
              </template>
              <span v-else class="text-dimmed">—</span>
            </td>
            <td class="px-3 py-2">
              <UBadge
                :color="request.state === 'approved' ? 'success' : request.state === 'denied' ? 'neutral' : 'info'"
                variant="subtle"
                size="sm"
              >
                {{ label(request) }}
              </UBadge>
              <p v-if="request.reason" class="text-xs text-dimmed mt-1">{{ request.reason }}</p>
              <p v-else-if="request.decidedBy" class="text-xs text-dimmed mt-1">by {{ request.decidedBy }}</p>
            </td>
            <td class="px-3 py-2 text-right whitespace-nowrap">
              <template v-if="mayDecide && decidable(request)">
                <UButton
                  v-if="request.state !== 'approved'"
                  color="neutral"
                  variant="link"
                  size="xs"
                  class="px-0 mr-3"
                  :loading="deciding === request.claim"
                  @click="decide(request, 'approved')"
                >
                  Admit
                </UButton>
                <UButton
                  v-if="request.state !== 'denied'"
                  color="neutral"
                  variant="link"
                  size="xs"
                  class="px-0"
                  @click="
                    denying = request;
                    denyReason = '';
                  "
                >
                  {{ request.state === "approved" ? "Withdraw" : "Refuse" }}
                </UButton>
              </template>
              <span v-else-if="mayDecide && request.visibility === 'open'" class="text-xs text-dimmed">
                the offering is open to every project
              </span>
              <span v-else-if="mayDecide" class="text-xs text-dimmed">
                this project no longer offers it
              </span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <p v-if="!mayDecide && readOnlyReason" class="text-xs text-dimmed">{{ readOnlyReason }}</p>

    <UModal
      :open="denying !== null"
      :title="denying ? `${denying.state === 'approved' ? 'Withdraw' : 'Refuse'} ${denying.project}?` : ''"
      @update:open="denying = null"
    >
      <template #body>
        <p class="text-sm text-muted">
          <template v-if="denying?.state === 'approved'">
            The address is taken back: the binding's secret is removed and
            <span class="font-mono">{{ denying?.project }}</span> rolls its environments without it.
          </template>
          <template v-else>
            <span class="font-mono">{{ denying?.project }}</span> stays bound to nothing. It can ask again on the same
            claim, without deleting it.
          </template>
        </p>
        <UFormField
          class="mt-4"
          label="Reason"
          description="Read on the other project's claim, and nowhere else — it holds no role here."
          required
        >
          <UInput v-model="denyReason" placeholder="we are retiring this offering" class="w-full" />
        </UFormField>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="ghost" @click="denying = null">Cancel</UButton>
          <UButton
            color="error"
            :disabled="!denyReason.trim()"
            :loading="deciding === denying?.claim"
            @click="denying && decide(denying, 'denied', denyReason.trim())"
          >
            {{ denying?.state === "approved" ? "Withdraw" : "Refuse" }}
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
