<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type Alert, type AlertsAnswer, type Audience, type MitigationRequest } from "../lib/api";
import {
  MAX_SILENCE_DAYS,
  audienceLabel,
  defaultSilenceUntil,
  mitigationSentence,
  silenced,
  sortAlerts,
  tierIcon,
  tierLabel,
  tierMeaning,
  tierTone,
} from "../lib/alerts";
import { timeAgo, uptime } from "../lib/format";
import { evidenceLabel, evidenceLocation, severityLabel } from "../lib/signals";

/**
 * The alerts list, and the controls on it.
 *
 * Both alerts screens are this component: the fleet's, which is everything the
 * reader may see, and a project's, which is the same rows narrowed to one
 * project. They are one component because **the same condition must not say
 * two different things depending on which list it is read from** — that was
 * the note left on the project's screen when it was shaped, and it is the
 * whole reason there is one of these.
 *
 * ## What a row shows, and in what order
 *
 * The **tier** first, because it is the question the screen answers: what am I
 * meant to do about this. Then what it is, then what is already being done
 * about it — a row nobody is acting on and a row somebody is are the two
 * states a triage is trying to tell apart, and until now they looked the same.
 *
 * ## What a row lets you do
 *
 * Acknowledge, silence, lift a silence, claim. Each is a record about *this*
 * delivery and never about the condition: acknowledging a project's row is
 * explicitly not acknowledging the operator's row about the same thing, which
 * is why the audience is on the row and goes back with every write.
 *
 * A **symptom** row is the exception and shows none of them. It is a platform
 * condition degrading this project, said in the project's own words — there is
 * nothing here the reader can press, and offering a button that answers 404
 * would be worse than offering none.
 */

const props = withDefaults(
  defineProps<{
    answer: AlertsAnswer | null;
    loading?: boolean;
    error?: string | null;
    /** Whether the reader may claim an escalated delivery. The API refuses
     * anybody else; the button is hidden rather than offered and refused. */
    canClaim?: boolean;
    /** What to say when there is genuinely nothing. */
    empty?: string;
  }>(),
  {
    loading: false,
    error: null,
    canClaim: false,
    empty: "Nothing needs acting on. Every rule in the catalogue was evaluated and none of them matched.",
  },
);

const emit = defineEmits<{ (event: "changed"): void }>();

const alerts = computed(() => sortAlerts(props.answer?.items));
/** Only a recorded answer's rows can be acted on: a delivery the platform
 * never wrote down has nothing for a record to be about. */
const recorded = computed(() => props.answer?.source === "recorded");

/** The row a write is in flight for, so two clicks cannot race. */
const busy = ref<string | null>(null);
const failed = ref<string | null>(null);

/** The delivery a silence is being composed for, and the form's two fields. */
const silencing = ref<Alert | null>(null);
const reason = ref("");
const until = ref(defaultSilenceUntil());

/** The modal's own open state, so that closing it clears the delivery it was
 * about rather than leaving a stale one behind the next time it opens. */
const silenceOpen = computed({
  get: () => silencing.value !== null,
  set: (open: boolean) => {
    if (!open) silencing.value = null;
  },
});

function key(alert: Alert): string {
  return `${alert.fingerprint}#${alert.audience}`;
}

function openSilence(alert: Alert): void {
  silencing.value = alert;
  reason.value = "";
  until.value = defaultSilenceUntil();
}

async function write(alert: Alert, call: (body: MitigationRequest) => Promise<unknown>, extra: Partial<MitigationRequest> = {}): Promise<void> {
  busy.value = key(alert);
  failed.value = null;
  try {
    await call({ fingerprint: alert.fingerprint, audience: alert.audience as Audience, ...extra });
    emit("changed");
  } catch (err) {
    failed.value = err instanceof Error ? err.message : String(err);
  } finally {
    busy.value = null;
  }
}

function acknowledge(alert: Alert): Promise<void> {
  return write(alert, api.ackAlert);
}

function claim(alert: Alert): Promise<void> {
  // A claim is about the operator's delivery and no other, so it carries no
  // audience at all.
  return write(alert, (body) => api.claimAlert({ fingerprint: body.fingerprint }));
}

function lift(alert: Alert): Promise<void> {
  return write(alert, api.unsilenceAlert);
}

async function confirmSilence(): Promise<void> {
  const alert = silencing.value;
  if (!alert) return;
  // `datetime-local` gives a local instant with no zone; the API wants RFC
  // 3339, and the browser's own offset is the honest reading of what the
  // person typed.
  const expiry = new Date(until.value);
  await write(alert, api.silenceAlert, { reason: reason.value.trim(), until: expiry.toISOString() });
  if (!failed.value) silencing.value = null;
}
</script>

<template>
  <div class="space-y-3">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <UAlert
      v-if="failed"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="failed"
      :close="{ onClick: () => (failed = null) }"
    />

    <!-- An answer with no history behind it is honest and thin: the conditions
         are right and nothing carries an age, so nothing on it can be
         acknowledged. Said once, above the list, rather than as a disabled
         button on every row. -->
    <UAlert
      v-if="answer?.message"
      color="warning"
      variant="soft"
      icon="i-lucide-eye-off"
      :title="answer.message"
    />

    <div class="rounded-md border border-default overflow-hidden">
      <div v-if="alerts.length" class="divide-y divide-muted">
        <div v-for="alert in alerts" :key="key(alert)" class="px-4 py-3 space-y-1.5">
          <div class="flex items-start gap-3">
            <UIcon
              :name="tierIcon(alert.tier)"
              class="size-4 shrink-0 mt-0.5"
              :class="{
                'text-error': tierTone(alert.tier) === 'error',
                'text-warning': tierTone(alert.tier) === 'warning',
                'text-dimmed': tierTone(alert.tier) === 'neutral',
              }"
              :title="tierMeaning(alert.tier)"
            />

            <div class="min-w-0 flex-1 space-y-1">
              <div class="flex items-baseline gap-2 flex-wrap text-sm">
                <span
                  class="font-medium"
                  :class="{
                    'text-error': tierTone(alert.tier) === 'error',
                    'text-warning': tierTone(alert.tier) === 'warning',
                    'text-dimmed': tierTone(alert.tier) === 'neutral',
                  }"
                >{{ tierLabel(alert.tier) }}</span>
                <span class="text-highlighted">{{ alert.title }}</span>
                <!-- The tier the rule declared, where something has moved it.
                     "A page, held down to a ticket because somebody is on it"
                     reads very differently from a ticket. -->
                <span v-if="alert.baseTier && alert.baseTier !== alert.tier" class="text-[11px] text-dimmed">
                  normally {{ tierLabel(alert.baseTier).toLowerCase() }}
                </span>
                <UBadge
                  v-if="!alert.symptom"
                  size="sm"
                  color="neutral"
                  variant="subtle"
                  :label="audienceLabel(alert.audience)"
                />
              </div>

              <p class="text-sm text-toned break-words">{{ alert.detail }}</p>

              <p class="text-[11px] text-dimmed font-mono truncate">
                {{ alert.symptom ? "" : alert.signal }}
                <template v-if="!alert.symptom && (alert.scope.project || alert.scope.environment)"> · </template>
                {{ [alert.scope.project, alert.scope.environment].filter(Boolean).join("/") }}
              </p>

              <!-- The escalation sentence, in the words the platform records
                   it in: `shop / production · unmitigated 4h 12m · nobody has
                   acknowledged`. -->
              <p v-if="alert.note" class="text-xs text-warning">{{ alert.note }}</p>

              <p v-if="mitigationSentence(alert)" class="text-xs text-muted">
                {{ mitigationSentence(alert) }}
              </p>
            </div>

            <span
              class="shrink-0 text-xs text-dimmed tabular-nums whitespace-nowrap"
              :title="alert.openedAt ? `first seen ${alert.openedAt}` : `firing since ${alert.since}`"
            >
              {{ uptime(alert.openedAt || alert.since) }}
            </span>
          </div>

          <div class="flex items-center gap-2 flex-wrap pl-7">
            <RouterLink
              v-if="evidenceLocation(alert.evidence)"
              :to="evidenceLocation(alert.evidence)!"
              class="text-xs text-primary hover:underline inline-flex items-center gap-1"
            >
              {{ evidenceLabel(alert.evidence) }}
              <UIcon name="i-lucide-arrow-right" class="size-3" />
            </RouterLink>

            <span class="text-[11px] text-dimmed">{{ severityLabel(alert.severity) }}</span>

            <template v-if="alert.actionable && recorded">
              <UButton
                v-if="!alert.mitigation?.acknowledged"
                size="xs"
                color="neutral"
                variant="subtle"
                icon="i-lucide-eye"
                label="I have seen this"
                :loading="busy === key(alert)"
                @click="acknowledge(alert)"
              />
              <UButton
                v-if="silenced(alert)"
                size="xs"
                color="neutral"
                variant="ghost"
                icon="i-lucide-bell"
                label="Lift the silence"
                :loading="busy === key(alert)"
                @click="lift(alert)"
              />
              <UButton
                v-else
                size="xs"
                color="neutral"
                variant="ghost"
                icon="i-lucide-bell-off"
                label="Quieten"
                @click="openSilence(alert)"
              />
              <UButton
                v-if="canClaim && alert.audience === 'operator' && !alert.mitigation?.claimedBy"
                size="xs"
                color="neutral"
                variant="ghost"
                icon="i-lucide-hand"
                label="I will take this"
                :loading="busy === key(alert)"
                @click="claim(alert)"
              />
            </template>
          </div>
        </div>
      </div>

      <p v-else class="px-4 py-3 text-xs text-muted flex items-center gap-2">
        <UIcon name="i-lucide-shield-check" class="size-4 text-success shrink-0" />
        <span>{{ loading && !answer ? "Evaluating…" : empty }}</span>
      </p>
    </div>

    <p v-if="answer?.evaluatedAt" class="text-[11px] text-dimmed">
      {{ answer.source === "recorded" ? "Recorded" : "Evaluated" }} {{ timeAgo(answer.evaluatedAt) }}
    </p>

    <UModal v-model:open="silenceOpen" :title="`Quieten ${silencing?.title ?? ''}`">
      <template #body>
        <div class="space-y-4">
          <p class="text-xs text-muted">
            This quietens the row you are reading and nothing else. The same condition as somebody else sees it is
            theirs, and stays where it is.
          </p>
          <UFormField label="Why" hint="Required" description="A reader months from now has only this sentence to go on.">
            <UInput v-model="reason" placeholder="waiting on the upstream fix" class="w-full" />
          </UFormField>
          <UFormField
            label="Until"
            hint="Required"
            :description="`At most ${MAX_SILENCE_DAYS} days. A decision nobody revisits within a month is one whose reason has quietly stopped being true.`"
          >
            <UInput v-model="until" type="datetime-local" class="w-full" />
          </UFormField>
        </div>
      </template>
      <template #footer>
        <div class="flex items-center justify-end gap-2 w-full">
          <UButton color="neutral" variant="ghost" label="Cancel" @click="silencing = null" />
          <UButton
            color="primary"
            label="Quieten"
            :disabled="!reason.trim() || !until"
            :loading="busy !== null"
            @click="confirmSilence"
          />
        </div>
      </template>
    </UModal>
  </div>
</template>
