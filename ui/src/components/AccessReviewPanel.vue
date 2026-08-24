<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type AccessDecision, type AccessReview, type AccessReviewEntry } from "../lib/api";
import { timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { useAsync } from "../lib/useAsync";

// Access recertification: who holds what here, and the cycle somebody is
// reviewing that against.
//
// The panel leads with the open cycle rather than with the register, because
// the open cycle is the thing that needs doing. Everything else — the history,
// and the live survey of grants between cycles — sits behind a toggle.
//
// Two things it says out loud rather than hiding, because they are the whole
// subject of the control: a decision about your own grant is marked as a
// self-review (recorded, never refused), and a grant that is dormant *and*
// unknown to the identity provider is marked orphaned. Neither blocks
// anything; both are things somebody has to look at.

const historical = ref(false);
const reviews = useAsync(() => api.accessReviews(historical.value));
const identities = useAsync(() => api.identities());
const showIdentities = ref(false);

function toggleHistorical() {
  historical.value = !historical.value;
  void reviews.refresh();
}

const rows = computed(() => reviews.data.value ?? []);
const open = computed(() => rows.value.find((review) => review.phase !== "Closed"));

const mayDecide = computed(() => may("PATCH /api/v1/access/reviews/{name}", callerFor()));
const mayOpen = computed(() => may("POST /api/v1/access/reviews", callerFor()));

const busy = ref("");
const actionError = ref("");

/** The pair identifies an entry: one account holding a role on four projects
 *  is four decisions, and a key of the subject alone would collapse them. */
function keyOf(entry: AccessReviewEntry): string {
  return `${entry.grant} ${entry.subject}`;
}

async function decide(review: AccessReview, entry: AccessReviewEntry, decision: "confirm" | "revoke") {
  let note = "";
  if (decision === "revoke") {
    const answer = window.prompt(
      `Revoking ${entry.role} on ${entry.grant} from ${entry.email || entry.subject} takes the grant off when ` +
        `this cycle closes. Why? (recorded in the audit log and in the cycle's artefact)`,
    );
    if (answer === null) return;
    note = answer.trim();
  }
  await send(review, { decisions: [{ subject: entry.subject, grant: entry.grant, decision, note }] }, keyOf(entry));
}

async function close(review: AccessReview) {
  const message =
    review.pending > 0
      ? `${review.pending} grant(s) are still undecided. Closing now records them as undecided in the cycle's ` +
        `artefact, which is exactly what an auditor reads it for. Close anyway?`
      : `Close ${review.name}? The revocations are carried out and the artefact is minted. ` +
        `A closed cycle cannot be reopened.`;
  if (!window.confirm(message)) return;
  await send(review, { close: true }, "close");
}

async function send(review: AccessReview, body: { decisions?: AccessDecision[]; close?: boolean }, token: string) {
  busy.value = token;
  actionError.value = "";
  try {
    await api.reviewAccess(review.name, body);
    void reviews.refresh();
  } catch (cause) {
    actionError.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    busy.value = "";
  }
}

async function openCycle() {
  const reason = window.prompt("Why open a recertification now? (recorded on the cycle and in the audit log)");
  if (reason === null) return;
  busy.value = "open";
  actionError.value = "";
  try {
    await api.openAccessReview({ scope: "all", reason: reason.trim() });
    void reviews.refresh();
  } catch (cause) {
    actionError.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    busy.value = "";
  }
}

function time(iso?: string): string {
  if (!iso) return "—";
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("en-GB");
}

function accountOf(entry: { subject: string; email?: string }): string {
  return entry.email || entry.subject;
}

const phaseTone: Record<string, string> = {
  Open: "text-info",
  Overdue: "text-warning",
  Closed: "text-dimmed",
};

/** What is odd about a grant, in the fewest words that stay accurate. */
function flagsOf(entry: { orphaned?: boolean; inactive?: boolean; unknown?: boolean }): string {
  if (entry.orphaned) return "orphaned";
  const words: string[] = [];
  if (entry.inactive) words.push("dormant");
  if (entry.unknown) words.push("no account");
  return words.join(", ");
}
</script>

<template>
  <div class="space-y-2">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <p class="text-sm text-highlighted font-medium">Access recertification</p>
        <p class="text-xs text-muted mt-0.5">
          Who holds what here, reviewed grant by grant on a cadence. Closing a cycle takes off what was revoked and
          leaves a signed, timestamped artefact of what was decided. An overdue cycle refuses nothing — it means
          somebody has to look.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <UButton
          size="xs"
          color="neutral"
          :variant="showIdentities ? 'solid' : 'subtle'"
          @click="showIdentities = !showIdentities"
        >
          {{ showIdentities ? "Hiding grants" : "Every grant" }}
        </UButton>
        <UButton size="xs" color="neutral" :variant="historical ? 'solid' : 'subtle'" @click="toggleHistorical">
          {{ historical ? "Showing history" : "Open only" }}
        </UButton>
        <UButton
          v-if="mayOpen && !open"
          size="xs"
          color="neutral"
          variant="subtle"
          :loading="busy === 'open'"
          @click="openCycle"
        >
          Open a cycle
        </UButton>
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="reviews.loading.value"
          aria-label="Refresh recertifications"
          @click="reviews.refresh"
        />
      </div>
    </div>

    <UAlert
      v-if="reviews.error.value"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="reviews.error.value"
    />
    <UAlert v-if="actionError" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="actionError" />

    <div v-if="!rows.length" class="rounded-md border border-default px-4 py-6 text-center text-sm text-muted">
      {{
        reviews.loading.value
          ? "Loading…"
          : historical
            ? "No recertification cycle has ever been opened here."
            : "No recertification is open. The platform opens one on its cadence; the history is behind the toggle."
      }}
    </div>

    <div v-for="review in rows" :key="review.name" class="rounded-md border border-default">
      <div class="px-4 py-3 flex items-start justify-between gap-4 flex-wrap border-b border-muted">
        <div>
          <p class="text-sm font-mono text-highlighted">
            {{ review.name }}
            <span class="ml-2 text-xs" :class="phaseTone[review.phase] ?? 'text-toned'">{{ review.phase }}</span>
          </p>
          <p class="text-xs text-muted mt-0.5">
            {{ review.scope === "all" ? "every grant on the installation" : review.scope }}
            <template v-if="review.project"> — {{ review.project }}</template>
            · opened by {{ review.openedBy }} · due {{ time(review.dueBy) }} ({{ timeAgo(review.dueBy) }})
          </p>
          <p class="text-xs text-toned mt-0.5">
            {{ review.pending }} undecided · {{ review.confirmed }} confirmed · {{ review.revoked }} revoked
            <template v-if="review.selfReviewed">
              · <span class="text-warning">{{ review.selfReviewed }} self-reviewed</span>
            </template>
            <template v-if="review.orphaned">
              · <span class="text-warning">{{ review.orphaned }} orphaned</span>
            </template>
          </p>
          <p v-if="review.closedBy" class="text-[11px] text-dimmed mt-0.5">
            Closed by {{ review.closedBy }} at {{ time(review.closedAt) }}.
            <template v-if="review.artifact?.recordID">
              Artefact {{ review.artifact.recordID }} signed {{ time(review.artifact.signedAt) }}.
            </template>
            <template v-else-if="review.artifact?.message">
              <span class="text-warning">{{ review.artifact.message }}</span>
            </template>
          </p>
        </div>
        <UButton
          v-if="mayDecide && review.phase !== 'Closed'"
          size="xs"
          color="neutral"
          variant="subtle"
          :loading="busy === 'close'"
          @click="close(review)"
        >
          Close the cycle
        </UButton>
      </div>

      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="text-left text-xs text-muted border-b border-muted bg-muted">
              <th class="px-3 py-2 font-medium">Account</th>
              <th class="px-3 py-2 font-medium">Grant</th>
              <th class="px-3 py-2 font-medium">Role</th>
              <th class="px-3 py-2 font-medium">Last active</th>
              <th class="px-3 py-2 font-medium">Decision</th>
              <th v-if="mayDecide && review.phase !== 'Closed'" class="px-3 py-2 font-medium"></th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="!review.entries.length">
              <td colspan="6" class="px-3 py-6 text-center text-muted text-sm">
                This cycle froze no grants: nobody held a role in its scope when it opened.
              </td>
            </tr>
            <tr
              v-for="entry in review.entries"
              :key="keyOf(entry)"
              class="border-b border-muted last:border-0 align-top hover:bg-elevated/40"
            >
              <td class="px-3 py-2 text-xs font-mono text-highlighted break-all">
                {{ accountOf(entry) }}
                <p v-if="flagsOf(entry)" class="text-[11px] text-warning font-sans">{{ flagsOf(entry) }}</p>
              </td>
              <td class="px-3 py-2 text-xs text-toned">{{ entry.grant }}</td>
              <td class="px-3 py-2 text-xs text-toned">{{ entry.role }}</td>
              <td class="px-3 py-2 text-xs text-dimmed whitespace-nowrap">
                {{ entry.lastActive ? timeAgo(entry.lastActive) : "never recorded" }}
              </td>
              <td class="px-3 py-2 text-xs">
                <span v-if="!entry.decision" class="text-dimmed">undecided</span>
                <span v-else :class="entry.decision === 'revoke' ? 'text-error' : 'text-success'">
                  {{ entry.decision }}
                </span>
                <span v-if="entry.selfReview" class="text-warning ml-1">(self-review)</span>
                <p v-if="entry.decidedBy" class="text-[11px] text-dimmed">by {{ entry.decidedBy }}</p>
                <p v-if="entry.note" class="text-[11px] text-toned break-words">{{ entry.note }}</p>
                <p v-if="entry.applyMessage" class="text-[11px] text-warning break-words">{{ entry.applyMessage }}</p>
              </td>
              <td v-if="mayDecide && review.phase !== 'Closed'" class="px-3 py-2 whitespace-nowrap">
                <div class="flex items-center gap-1">
                  <UButton
                    size="xs"
                    color="neutral"
                    variant="ghost"
                    :loading="busy === keyOf(entry)"
                    @click="decide(review, entry, 'confirm')"
                  >
                    Confirm
                  </UButton>
                  <UButton
                    size="xs"
                    color="error"
                    variant="ghost"
                    :loading="busy === keyOf(entry)"
                    @click="decide(review, entry, 'revoke')"
                  >
                    Revoke
                  </UButton>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- The live survey, between cycles: every grant as it stands now, from
         the same materializer a cycle freezes — so this table and a snapshot
         cannot disagree about what the platform's access is. -->
    <div v-if="showIdentities" class="rounded-md border border-default">
      <div class="px-4 py-3 border-b border-muted">
        <p class="text-sm text-highlighted font-medium">Every grant, right now</p>
        <p class="text-xs text-muted mt-0.5">
          One row per grant, not per account. Orphaned means dormant <em>and</em> unknown to the identity provider —
          either alone has an innocent reading, the pair does not. Dormancy is measured from the audit log, which
          records writes: somebody who only ever reads looks dormant and is not.
        </p>
        <p v-if="identities.data.value && !identities.data.value.directoryConsulted" class="text-xs text-warning mt-1">
          The account directory did not answer, so nothing here is reported as belonging to nobody.
        </p>
        <p v-if="identities.data.value?.message" class="text-xs text-warning mt-1">
          {{ identities.data.value.message }}
        </p>
      </div>
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="text-left text-xs text-muted border-b border-muted bg-muted">
              <th class="px-3 py-2 font-medium">Account</th>
              <th class="px-3 py-2 font-medium">Grant</th>
              <th class="px-3 py-2 font-medium">Role</th>
              <th class="px-3 py-2 font-medium">Last active</th>
              <th class="px-3 py-2 font-medium">Flags</th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="!identities.data.value?.identities.length">
              <td colspan="5" class="px-3 py-6 text-center text-muted text-sm">
                {{ identities.loading.value ? "Loading…" : "Nobody holds a role here." }}
              </td>
            </tr>
            <tr
              v-for="row in identities.data.value?.identities ?? []"
              :key="`${row.grant}/${row.subject}`"
              class="border-b border-muted last:border-0 hover:bg-elevated/40"
            >
              <td class="px-3 py-2 text-xs font-mono text-highlighted break-all">{{ accountOf(row) }}</td>
              <td class="px-3 py-2 text-xs text-toned">{{ row.grant }}</td>
              <td class="px-3 py-2 text-xs text-toned">{{ row.role }}</td>
              <td class="px-3 py-2 text-xs text-dimmed whitespace-nowrap">
                {{ row.lastActive ? timeAgo(row.lastActive) : "never recorded" }}
              </td>
              <td class="px-3 py-2 text-xs" :class="row.orphaned ? 'text-warning' : 'text-dimmed'">
                {{ flagsOf(row) || "—" }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
