<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type NotificationDelivery, type NotificationSubscription } from "../lib/api";
import { timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import {
  MIN_SECRET_LENGTH,
  NOTIFICATION_EVENTS,
  deliveryTone,
  deliveryWords,
  eventLabel,
  eventSummary,
  generateSecret,
  subscriptionState,
  urlProblem,
} from "../lib/notifications";
import { may } from "../lib/policy";
import { useAsync, usePoll } from "../lib/useAsync";
import StatusDot from "./StatusDot.vue";

// Where this project — or this platform — sends an account of itself.
//
// The platform posts one shape of payload, signed, to an address somebody
// chose; a chat app's message is a small relay in front of it. So this panel
// is about three things and no others: which events go where, whether they are
// arriving, and what to do about the ones that never did.
//
// `project` decides the scope, and the scope decides everything else: with a
// project this is that project's own subscriptions, on its settings; without
// one it is the platform's screen, where every subscription on the
// installation is listed because "every address this sends activity to" is one
// question and it is the one an operator asks.
const props = defineProps<{ project?: string; role?: string }>();

const toast = useToast();

const caller = computed(() => callerFor(props.role));
const maySubscribe = computed(() => may("POST /api/v1/notifications/subscriptions", caller.value));
const mayChange = computed(() => may("PATCH /api/v1/notifications/subscriptions/{name}", caller.value));
const mayDelete = computed(() => may("DELETE /api/v1/notifications/subscriptions/{name}", caller.value));
const mayRetry = computed(() => may("POST /api/v1/notifications/deliveries/{name}/retry", caller.value));

const subscriptions = useAsync(() => api.subscriptions(props.project ? { project: props.project } : undefined));
const deliveries = useAsync(() => api.deliveries());

watch(
  () => props.project,
  () => {
    void subscriptions.refresh();
    void deliveries.refresh();
  },
);

const entries = computed<NotificationSubscription[]>(() => subscriptions.data.value ?? []);
const sent = computed<NotificationDelivery[]>(() => deliveries.data.value ?? []);

/** A delivery still on the ladder is a delivery that will move on its own. */
const inFlight = computed(() => sent.value.some((delivery) => delivery.phase === "Pending"));
usePoll(
  () => {
    void subscriptions.refresh();
    void deliveries.refresh();
  },
  10000,
  () => inFlight.value,
);

/** The ones that never arrived, newest first — what the panel exists to make
 * findable. */
const undelivered = computed(() => sent.value.filter((delivery) => delivery.phase === "DeadLettered"));

function deliveriesOf(subscription: string): NotificationDelivery[] {
  return sent.value.filter((delivery) => delivery.subscription === subscription).slice(0, 10);
}

const expanded = ref<string | null>(null);
function toggle(name: string) {
  expanded.value = expanded.value === name ? null : name;
}

// Adding one. The signing key is generated here and shown once: the API never
// reads a credential back, so the only copy that matters is the one going into
// whatever receives these.
const adding = ref(false);
const name = ref("");
const url = ref("");
const description = ref("");
const chosen = ref<string[]>(["deploy.succeeded", "build.failed"]);
const secret = ref("");
const saving = ref(false);

const addressProblem = computed(() => urlProblem(url.value));
const complete = computed(
  () => name.value.trim() !== "" && url.value.trim() !== "" && !addressProblem.value && chosen.value.length > 0,
);

watch(adding, (open) => {
  if (!open) return;
  name.value = props.project ? `${props.project}-relay` : "platform-relay";
  url.value = "";
  description.value = "";
  chosen.value = ["deploy.succeeded", "build.failed"];
  secret.value = generateSecret();
});

function toggleEvent(value: string) {
  chosen.value = chosen.value.includes(value)
    ? chosen.value.filter((event) => event !== value)
    : [...chosen.value, value];
}

async function copy(label: string, value: string) {
  try {
    await navigator.clipboard.writeText(value);
    toast.add({ title: `${label} copied`, color: "success", icon: "i-lucide-clipboard-check" });
  } catch (err) {
    toast.add({ title: "Copy failed", description: err instanceof Error ? err.message : String(err), color: "error" });
  }
}

function failed(title: string, err: unknown) {
  toast.add({ title, description: err instanceof Error ? err.message : String(err), color: "error" });
}

async function add() {
  if (!complete.value || saving.value) return;
  saving.value = true;
  try {
    await api.createSubscription({
      name: name.value.trim(),
      url: url.value.trim(),
      events: chosen.value,
      project: props.project,
      description: description.value.trim() || undefined,
      secret: secret.value,
    });
    toast.add({ title: `${name.value.trim()} subscribed`, color: "success", icon: "i-lucide-bell" });
    adding.value = false;
    await subscriptions.refresh();
  } catch (err) {
    failed("Subscribing failed", err);
  } finally {
    saving.value = false;
  }
}

// Pausing, rotating and removing: the three things done to one that exists.
async function setPaused(subscription: NotificationSubscription, paused: boolean) {
  try {
    await api.patchSubscription(subscription.name, { suspended: paused });
    toast.add({
      title: paused ? `${subscription.name} paused` : `${subscription.name} resumed`,
      description: paused ? "Nothing new is queued; anything already queued waits." : undefined,
      color: "success",
      icon: paused ? "i-lucide-pause" : "i-lucide-play",
    });
    await subscriptions.refresh();
  } catch (err) {
    failed("The change was refused", err);
  }
}

const rotating = ref<NotificationSubscription | null>(null);
const rotated = ref("");
const rotatingNow = ref(false);
watch(rotating, (subscription) => {
  if (subscription) rotated.value = generateSecret();
});

async function rotate() {
  const subscription = rotating.value;
  if (!subscription || rotatingNow.value) return;
  rotatingNow.value = true;
  try {
    await api.patchSubscription(subscription.name, { secret: rotated.value });
    toast.add({
      title: "Signing key replaced",
      description: "Payloads are signed with the new key from the next attempt.",
      color: "success",
      icon: "i-lucide-key-round",
    });
    rotating.value = null;
    await subscriptions.refresh();
  } catch (err) {
    failed("Rotating the key failed", err);
  } finally {
    rotatingNow.value = false;
  }
}

const removing = ref<NotificationSubscription | null>(null);
const deleting = ref(false);
async function remove() {
  const subscription = removing.value;
  if (!subscription) return;
  deleting.value = true;
  try {
    await api.deleteSubscription(subscription.name);
    toast.add({ title: `${subscription.name} removed`, color: "success", icon: "i-lucide-trash-2" });
    removing.value = null;
    await Promise.all([subscriptions.refresh(), deliveries.refresh()]);
  } catch (err) {
    failed("Removing it failed", err);
  } finally {
    deleting.value = false;
  }
}

const retrying = ref<string | null>(null);
async function retry(delivery: NotificationDelivery) {
  retrying.value = delivery.name;
  try {
    await api.retryDelivery(delivery.name);
    toast.add({
      title: "Sending it again",
      description: "The same message, under the same id — a receiver that did get it can ignore the repeat.",
      color: "success",
      icon: "i-lucide-rotate-ccw",
    });
    await deliveries.refresh();
  } catch (err) {
    failed("It could not be sent again", err);
  } finally {
    retrying.value = null;
  }
}
</script>

<template>
  <div>
    <div class="flex items-center justify-between mb-2">
      <h2 class="text-sm font-medium text-highlighted">Notifications</h2>
      <UButton
        v-if="maySubscribe"
        color="neutral"
        variant="subtle"
        size="xs"
        icon="i-lucide-plus"
        @click="adding = true"
      >
        Send somewhere
      </UButton>
    </div>
    <p class="text-xs text-muted mb-3">
      <template v-if="project">
        Post this project's deploys, failures and previews to an address of your own. Each message is signed, retried
        if it is refused, and kept here if it never arrives.
      </template>
      <template v-else>
        Every address this platform posts its activity to. Each message is signed with that subscription's own key,
        retried if it is refused, and kept here if it never arrives.
      </template>
    </p>

    <UAlert
      v-if="subscriptions.error.value"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="subscriptions.error.value"
    />
    <div v-else class="rounded-md border border-default divide-y divide-default overflow-hidden">
      <p v-if="!entries.length" class="px-4 py-3 text-sm text-muted">
        Nothing is being told. A failed build at 02:00 waits for whoever opens this next.
      </p>
      <div v-for="entry in entries" :key="entry.name">
        <div
          class="flex items-center gap-3 px-4 py-2.5 text-sm cursor-pointer hover:bg-elevated"
          @click="toggle(entry.name)"
        >
          <StatusDot :tone="subscriptionState(entry).tone" />
          <span class="font-mono text-highlighted truncate" :title="entry.url">{{ entry.url }}</span>
          <UBadge v-if="!project && entry.scope === 'platform'" color="neutral" variant="subtle" size="sm">
            everything
          </UBadge>
          <UBadge v-else-if="!project" color="neutral" variant="subtle" size="sm">{{ entry.project }}</UBadge>
          <span class="text-xs text-muted truncate">{{ subscriptionState(entry).words }}</span>
          <span class="ml-auto text-xs text-dimmed whitespace-nowrap">{{ entry.events.length }} event(s)</span>
          <UButton
            v-if="mayChange"
            color="neutral"
            variant="ghost"
            size="xs"
            :icon="entry.suspended ? 'i-lucide-play' : 'i-lucide-pause'"
            :aria-label="entry.suspended ? `Resume ${entry.name}` : `Pause ${entry.name}`"
            @click.stop="setPaused(entry, !entry.suspended)"
          />
          <UButton
            v-if="mayChange"
            color="neutral"
            variant="ghost"
            size="xs"
            icon="i-lucide-key-round"
            :aria-label="`Replace the signing key for ${entry.name}`"
            @click.stop="rotating = entry"
          />
          <UButton
            v-if="mayDelete"
            color="neutral"
            variant="ghost"
            size="xs"
            icon="i-lucide-trash-2"
            :aria-label="`Remove ${entry.name}`"
            @click.stop="removing = entry"
          />
        </div>

        <div v-if="expanded === entry.name" class="px-4 py-3 bg-muted space-y-3 border-t border-muted">
          <p v-if="entry.description" class="text-xs text-toned">{{ entry.description }}</p>
          <p class="text-xs text-muted">Sends: {{ eventSummary(entry) }}</p>
          <p class="text-xs text-dimmed">
            {{ entry.delivered }} delivered · {{ entry.failed }} failed attempts ·
            {{ entry.deadLettered }} never arrived · up to {{ entry.maxAttempts }} attempts,
            {{ entry.timeoutSeconds }}s each
            <template v-if="entry.createdBy"> · added by {{ entry.createdBy }}</template>
          </p>
          <p v-if="!entry.ready && entry.reason" class="text-xs text-error">{{ entry.reason }}</p>

          <div class="overflow-x-auto">
            <table class="w-full min-w-[32rem] text-xs">
              <thead>
                <tr class="text-dimmed text-left">
                  <th class="font-medium py-1">Message</th>
                  <th class="font-medium py-1">What happened</th>
                  <th class="font-medium py-1 text-right">When</th>
                  <th class="font-medium py-1"></th>
                </tr>
              </thead>
              <tbody>
                <tr v-if="!deliveriesOf(entry.name).length">
                  <td colspan="4" class="py-8 text-center text-muted">Nothing has been sent here yet.</td>
                </tr>
                <tr v-for="delivery in deliveriesOf(entry.name)" :key="delivery.name" class="border-t border-default">
                  <td class="py-1 text-toned">{{ eventLabel(delivery.event) }}</td>
                  <td class="py-1">
                    <span class="inline-flex items-center gap-1.5">
                      <StatusDot :tone="deliveryTone(delivery)" :pulse="delivery.phase === 'Pending'" />
                      <span class="text-muted">{{ deliveryWords(delivery) }}</span>
                    </span>
                  </td>
                  <td class="py-1 text-right text-dimmed whitespace-nowrap">{{ timeAgo(delivery.queuedAt) }}</td>
                  <td class="py-1 text-right">
                    <UButton
                      v-if="mayRetry && delivery.phase === 'DeadLettered'"
                      color="neutral"
                      variant="ghost"
                      size="xs"
                      icon="i-lucide-rotate-ccw"
                      :loading="retrying === delivery.name"
                      :aria-label="`Send ${delivery.event} again`"
                      @click="retry(delivery)"
                    >
                      Send again
                    </UButton>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </div>

    <p v-if="undelivered.length" class="text-xs text-warning mt-2">
      {{ undelivered.length }} message(s) never arrived. Open the subscription to read the reason and send them again.
    </p>

    <!-- Adding one: the address, what it hears, and the key it verifies with. -->
    <UModal
      :open="adding"
      title="Send notifications somewhere"
      description="Every message is a signed POST of one JSON body. Anything vendor-shaped — a chat message, a page — is a small relay in front of this."
      @update:open="(open: boolean) => { adding = open; }"
    >
      <template #body>
        <form class="space-y-4" @submit.prevent="add">
          <UFormField label="Name" help="What this subscription is called here." required>
            <UInput v-model="name" class="w-full font-mono" autofocus />
          </UFormField>
          <UFormField
            label="Address"
            :help="addressProblem || 'Where the messages are posted. It must be https.'"
            :error="addressProblem || undefined"
            required
          >
            <UInput v-model="url" placeholder="https://relay.example.com/kitchen" class="w-full font-mono" />
          </UFormField>
          <UFormField label="What it hears" required>
            <div class="space-y-1.5">
              <label
                v-for="event in NOTIFICATION_EVENTS"
                :key="event.value"
                class="flex items-start gap-2 text-sm cursor-pointer"
              >
                <UCheckbox
                  :model-value="chosen.includes(event.value)"
                  @update:model-value="toggleEvent(event.value)"
                />
                <span>
                  <span class="text-toned">{{ event.label }}</span>
                  <span class="block text-xs text-dimmed">{{ event.help }}</span>
                </span>
              </label>
            </div>
          </UFormField>
          <UFormField label="What it is for" hint="optional">
            <UInput v-model="description" placeholder="into the deploy channel" class="w-full" />
          </UFormField>
          <UFormField
            label="Signing key"
            :help="`Every message is signed with this. Copy it into whatever receives them — the platform never shows it again. At least ${MIN_SECRET_LENGTH} characters.`"
          >
            <div class="flex items-center gap-2">
              <UInput v-model="secret" class="w-full font-mono text-xs" />
              <UButton
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-copy"
                aria-label="Copy the signing key"
                @click="copy('Signing key', secret)"
              />
            </div>
          </UFormField>
        </form>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="ghost" @click="adding = false">Cancel</UButton>
          <UButton :disabled="!complete" :loading="saving" icon="i-lucide-bell" @click="add">Subscribe</UButton>
        </div>
      </template>
    </UModal>

    <!-- Rotating the key. The old one stops working the moment this is saved. -->
    <UModal
      :open="rotating !== null"
      title="Replace the signing key"
      description="The next message is signed with the new key. Copy it into the receiver first — the platform never shows it again, and a receiver still checking the old one will refuse everything."
      @update:open="(open: boolean) => { if (!open) rotating = null; }"
    >
      <template #body>
        <div class="flex items-center gap-2">
          <UInput v-model="rotated" class="w-full font-mono text-xs" />
          <UButton
            color="neutral"
            variant="ghost"
            size="xs"
            icon="i-lucide-copy"
            aria-label="Copy the new signing key"
            @click="copy('Signing key', rotated)"
          />
        </div>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="rotating = null">Cancel</UButton>
          <UButton :loading="rotatingNow" icon="i-lucide-key-round" @click="rotate">Replace it</UButton>
        </div>
      </template>
    </UModal>

    <!-- Removing one. -->
    <UModal
      :open="removing !== null"
      :title="`Stop sending to ${removing?.name}?`"
      description="The address, its signing key and everything sent to it are deleted. Nothing that was already delivered is recalled."
      @update:open="(open: boolean) => { if (!open) removing = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="removing = null">Cancel</UButton>
          <UButton color="error" :loading="deleting" icon="i-lucide-trash-2" @click="remove">Remove it</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
