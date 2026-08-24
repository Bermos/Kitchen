<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type VEXAnswer, type VEXStatement } from "../lib/api";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";

// Exploitability assertions, beside the findings they modify.
//
// This panel exists because of one acceptance criterion and it is the one
// worth writing a screen for: a suppressed finding must never be invisible. A
// scanner that reports two hundred findings of which four matter is a control
// people stop reading; a policy that quietly drops a hundred and ninety-six of
// them is a control nobody can audit. So the findings are listed in full, and
// each one that somebody has asserted something about carries the assertion,
// its author, and what the platform can establish about it.
//
// What it never says is "suppressed". Whether a statement suppresses anything
// is the target environment's policy's question — its bundle decides whose
// word it takes, whether an unverified statement counts, and how old one may
// be — and the same statement can be honoured in staging and refused in
// production. This panel reports facts about the artifact, which is the same
// line the gates panel holds.

const props = defineProps<{ build: string; project?: string; role?: string }>();
const toast = useToast();

const answer = ref<VEXAnswer | null>(null);
const error = ref("");
const loading = ref(false);

// Filing an assertion is an **admin's** write, which is the one place this
// parts company with the gates it otherwise resembles: a gate result is a fact
// about an artifact, and a not_affected statement is an assertion whose effect
// is to stop a finding counting. The API enforces it; this only decides
// whether to draw the form, so a developer is not offered a control that would
// be refused.
const caller = computed(() => callerFor(props.role, props.project));
const mayAssert = computed(() => may("POST /api/v1/builds/{name}/vex", caller.value));

const composing = ref(false);
const draft = ref("");
const submitting = ref(false);

// Read on demand, like the evidence panel: the answer comes from the registry
// and is not worth a round trip on every build anybody opens.
async function load() {
  loading.value = true;
  error.value = "";
  try {
    answer.value = await api.vex(props.build);
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    loading.value = false;
  }
}

// Submitting sends the document as the exact bytes whoever wrote it wrote: the
// platform signs those bytes, and re-encoding somebody's assertion into a
// shape of this dashboard's choosing would be the dashboard editing it. So the
// textarea's contents are parsed only to fail early on JSON that is not JSON,
// and the raw text is what goes out.
async function submit() {
  let document: unknown;
  try {
    document = JSON.parse(draft.value);
  } catch (cause) {
    error.value = `That is not JSON: ${cause instanceof Error ? cause.message : String(cause)}`;
    return;
  }
  submitting.value = true;
  error.value = "";
  try {
    const accepted = await api.submitVEX(props.build, document);
    toast.add({
      title: "Assertion filed",
      description: `Attributed to you, authored by ${accepted.author}, covering ${accepted.vulnerabilities.join(", ")}.`,
      color: "success",
      icon: "i-lucide-shield-check",
    });
    draft.value = "";
    composing.value = false;
    await load();
  } catch (cause) {
    // The refusals are the interesting half — an unjustified not_affected, an
    // author the platform does not admit, a platform holding no key — so they
    // are shown where the form is rather than in a toast that scrolls away.
    error.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    submitting.value = false;
  }
}

/** What the platform can establish about a statement, in one word. */
function state(statement: VEXStatement): string {
  if (statement.expired) return "expired";
  if (!statement.verified) return "unverified";
  if (statement.status === "not_affected" && !statement.justified) return "unjustified";
  return "current";
}

function tone(statement: VEXStatement): string {
  return state(statement) === "current" ? "text-success" : "text-warning";
}

function severityTone(severity?: string): string {
  switch ((severity ?? "").toLowerCase()) {
    case "critical":
    case "high":
      return "text-error";
    case "medium":
      return "text-warning";
    default:
      return "text-dimmed";
  }
}

/** Why a statement is not one a policy would act on, said in a sentence
 *  rather than left to the reader to work out from a badge. */
function caveat(statement: VEXStatement): string {
  if (statement.expired) {
    return `This assertion expired${statement.expiresAt ? ` on ${new Date(statement.expiresAt).toLocaleDateString("en-GB")}` : ""}, so the finding counts again.`;
  }
  if (!statement.verified) {
    return "No key this platform holds accepted this document's signature, so a policy that requires a verified statement will not honour it.";
  }
  if (statement.status === "not_affected" && !statement.justified) {
    return "not_affected without a justification from the OpenVEX enumeration suppresses nothing: free text belongs beside a justification, not instead of one.";
  }
  return "";
}
</script>

<template>
  <div class="rounded-md border border-default px-5 py-4 space-y-3">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <p class="text-sm font-medium text-highlighted">Exploitability (VEX)</p>
        <p class="text-xs text-muted mt-0.5">
          What has been asserted about this artifact's findings applying here — the component is not present, the
          vulnerable code is not reachable, a mitigation is already in place. Nothing is applied silently: every
          finding is listed, with the assertion covering it and whoever made it.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <UButton
          v-if="mayAssert"
          size="xs"
          color="neutral"
          :variant="composing ? 'solid' : 'subtle'"
          @click="composing = !composing"
        >
          {{ composing ? "Cancel" : "File an assertion" }}
        </UButton>
        <UButton size="xs" color="neutral" variant="subtle" :loading="loading" @click="load">
          {{ answer ? "Re-read the assertions" : "Read the assertions" }}
        </UButton>
      </div>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <!-- The document goes out as written. What the platform adds is
         attribution — who submitted it, recorded on the build and in the audit
         log — and a signature meaning these bytes were submitted by that
         identity at that moment. It is not a claim that the assertion is
         true. -->
    <div v-if="composing" class="space-y-2 rounded border border-default px-3 py-3">
      <p class="text-xs text-muted">
        Paste an OpenVEX document. It is signed and attached exactly as written, and recorded against your name.
        A <span class="font-mono">not_affected</span> statement must give one of OpenVEX's five justifications —
        free text alone is refused, because a suppression whose reason cannot be counted cannot be reviewed.
      </p>
      <UTextarea
        v-model="draft"
        :rows="10"
        class="w-full font-mono text-xs"
        placeholder='{"@context": "https://openvex.dev/ns/v0.2.0", "author": "…", "statements": [ … ]}'
      />
      <div class="flex items-center gap-2">
        <UButton size="xs" color="primary" :loading="submitting" :disabled="!draft.trim()" @click="submit">
          Sign and attach
        </UButton>
        <span class="text-[11px] text-dimmed">Attached to the artifact's digest, not to this build.</span>
      </div>
    </div>

    <!-- v-if rather than v-else-if on the alert: an error from a refused
         submission must not blank the listing the reader was looking at. -->
    <template v-if="answer">
      <UAlert v-if="answer.caveat" color="warning" variant="soft" icon="i-lucide-shield-question" :title="answer.caveat" />

      <p v-if="!answer.findings.length && !answer.statements.length" class="text-xs text-muted">
        Nothing has been asserted about this artifact, and nothing has scanned it since it was built. An empty view
        here is not the same answer as nothing being wrong.
      </p>

      <div v-if="answer.findings.length" class="space-y-2">
        <div
          v-for="finding in answer.findings"
          :key="finding.vulnerability + (finding.package ?? '')"
          class="rounded border border-default px-3 py-2"
        >
          <p class="text-xs flex items-center gap-2 flex-wrap">
            <span class="font-mono text-highlighted">{{ finding.vulnerability }}</span>
            <span :class="severityTone(finding.severity)">{{ finding.severity ?? "unknown" }}</span>
            <span v-if="finding.package" class="text-dimmed">{{ finding.package }}<span v-if="finding.version">@{{ finding.version }}</span></span>
            <UBadge v-if="finding.vex" :color="state(finding.vex) === 'current' ? 'success' : 'warning'" variant="subtle" size="sm">
              {{ finding.vex.status }}
            </UBadge>
          </p>
          <p v-if="finding.vex" class="text-[11px] mt-1" :class="tone(finding.vex)">
            {{ finding.vex.justification || "no justification given" }}
            <span class="text-dimmed">
              · asserted by {{ finding.vex.author || "somebody unnamed" }}
              <template v-if="finding.vex.submittedBy">, submitted by {{ finding.vex.submittedBy }}</template>
              <template v-if="finding.vex.timestamp">, {{ new Date(finding.vex.timestamp).toLocaleDateString("en-GB") }}</template>
            </span>
          </p>
          <p v-if="finding.vex && caveat(finding.vex)" class="text-[11px] text-warning mt-1">
            {{ caveat(finding.vex) }}
          </p>
          <p v-if="finding.vex?.impactStatement" class="text-[11px] text-dimmed mt-1">
            {{ finding.vex.impactStatement }}
          </p>
          <p v-if="!finding.vex && finding.fixedIn" class="text-[11px] text-dimmed mt-1">fixed in {{ finding.fixedIn }}</p>
        </div>
      </div>

      <!-- Statements about vulnerabilities nothing has found here. They are
           worth showing: a vendor's assertion that arrived before the scanner
           did is the ordinary case, not an anomaly. -->
      <div v-if="answer.statements.length" class="space-y-1">
        <p class="text-[11px] text-dimmed uppercase tracking-wide">All statements</p>
        <div
          v-for="statement in answer.statements"
          :key="(statement.documentID ?? '') + statement.vulnerability + statement.status"
          class="text-[11px] flex items-center gap-2 flex-wrap"
        >
          <span class="font-mono text-toned">{{ statement.vulnerability }}</span>
          <span class="text-muted">{{ statement.status }}</span>
          <span v-if="statement.justification" class="text-dimmed">{{ statement.justification }}</span>
          <span class="text-dimmed">· {{ statement.author || "unnamed" }}</span>
          <span :class="tone(statement)">{{ state(statement) }}</span>
        </div>
      </div>

      <p class="text-[11px] text-dimmed leading-relaxed">
        The documents are attached to the artifact's digest under OpenVEX's own predicate type, so
        <span class="font-mono">cosign download attestation</span> reads them back with this platform out of the loop.
        Whether a statement suppresses a finding is decided by the environment being deployed to, not here.
      </p>
    </template>
  </div>
</template>
