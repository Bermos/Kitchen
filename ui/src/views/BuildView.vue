<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { api, type EvidenceSet, type LogLine, type LogQuery, type QualityGate } from "../lib/api";
import { buildStallLine } from "../lib/builds";
import { duration, shortSHA, timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { useAsync, usePoll } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";
import LogViewer from "../components/LogViewer.vue";
import OperatorOnly from "../components/OperatorOnly.vue";
import CommitBody from "../components/CommitBody.vue";
import PageHeader from "../components/PageHeader.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
import VEXPanel from "../components/VEXPanel.vue";

const route = useRoute();
const toast = useToast();
const name = computed(() => route.params.name as string);

const { data: build, error, loading, refresh } = useAsync(() => api.build(name.value));
watch(name, () => void refresh());

// Cancelling is the project developer's, and a Build carries no role of its
// own — the project it belongs to does. It is fetched apart from the build so
// that the log, which is what this page is usually open for, does not wait on
// it; until it answers there is no cancel button, which is the right way round
// for a control that would otherwise be refused.
const project = useAsync(() => api.project(build.value!.project), { immediate: false });
watch(build, (loaded, previous) => {
  if (loaded && loaded.project !== previous?.project) void project.refresh();
});
const mayCancel = computed(() =>
  may("POST /api/v1/builds/{name}/cancel", callerFor(project.data.value?.role, build.value?.project)),
);

// A queued or running build is still moving; keep the header fresh while the
// log viewer below follows the output.
const moving = computed(() => build.value?.phase === "Queued" || build.value?.phase === "Running");
usePoll(() => void refresh(), 5000, () => moving.value);

// Cancelling keeps the Build — phase Cancelled — and stops its BuildKit job.
const cancelling = ref(false);
async function cancel() {
  cancelling.value = true;
  try {
    await api.cancelBuild(name.value);
    toast.add({ title: `Build ${name.value} cancelled`, color: "success", icon: "i-lucide-ban" });
    await refresh();
  } catch (err) {
    toast.add({
      title: "Cancelling the build failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    cancelling.value = false;
  }
}

// The evidence attached to what this build produced. It is fetched on demand
// rather than with the build: it is a round trip to the registry, and a build
// page is opened far more often to read the log than to check the signature.
//
// Nothing here is stored on the Build. The attestations live in the registry
// against the artifact's digest and are readable with cosign — this panel is a
// convenience over that, not the record itself.
const evidence = ref<EvidenceSet | null>(null);
const evidenceError = ref("");
const loadingEvidence = ref(false);

async function loadEvidence() {
  loadingEvidence.value = true;
  evidenceError.value = "";
  try {
    evidence.value = await api.attestations(name.value);
  } catch (cause) {
    evidenceError.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    loadingEvidence.value = false;
  }
}

// A different build is a different artifact, so whatever was on screen is
// about something else now.
watch(name, () => {
  evidence.value = null;
  evidenceError.value = "";
});

// What the layer cache did, in the words the duration needs next to it. A
// cold build is not a fault — it is a build that had nothing to reuse, and
// saying so is what keeps it from reading as a regression.
const cacheNote = computed(() => {
  const cache = build.value?.cache;
  if (!cache) return null;
  if (!cache.enabled) {
    return { label: cache.message ? "no cache" : "cache off", tone: "text-dimmed", detail: cache.message ?? "" };
  }
  return cache.warm
    ? { label: "cache warm", tone: "text-success", detail: cache.ref ?? "" }
    : { label: "cache cold", tone: "text-warning", detail: cache.message || (cache.ref ?? "") };
});

/** Enough of a digest to recognise it by; the whole thing is in the title. */
function shortDigest(digest?: string): string {
  const hex = digest?.split(":")[1];
  return hex ? hex.slice(0, 12) : "—";
}

/** The last path segment of a predicate type — `build-record/v1` out of the
 *  URI — which is what distinguishes one piece of evidence from another. */
function predicateLabel(uri: string): string {
  return uri.replace(/^https?:\/\//, "");
}

/** What to call each kind of evidence on screen. The API sends the label
 *  alongside the URI, so this maps labels rather than parsing URIs — a
 *  predicate type this build of the dashboard has never heard of still lands
 *  somewhere honest instead of being dropped. */
const evidenceLabels: Record<string, string> = {
  provenance: "provenance",
  sbom: "SBOM",
  buildRecord: "build record",
  deployment: "deployment",
  other: "attestation",
};

function evidenceLabel(kind: string): string {
  return evidenceLabels[kind] ?? evidenceLabels.other;
}

/** How a gate's run reads at a glance.
 *
 *  "Completed" is not "passed" and is never drawn as though it were: the icon
 *  says the gate ran, and only a policy — which lives a phase away — decides
 *  what its findings mean. The one thing worth warning about here is a gate
 *  that did not run, because that is the state a compliance system can sit in
 *  while looking green. */
function gateIcon(ran: QualityGate): string {
  if (ran.phase === "Failed") return "i-lucide-triangle-alert";
  if (ran.phase === "Completed") return "i-lucide-clipboard-check";
  return "i-lucide-loader";
}

function gateTone(ran: QualityGate): string {
  if (ran.phase === "Failed") return "text-warning";
  if (ran.phase === "Completed") return "text-toned";
  return "text-dimmed";
}

function gateBadge(ran: QualityGate): "warning" | "neutral" {
  return ran.phase === "Failed" ? "warning" : "neutral";
}

// Why the build failed, in the words of whatever failed.
//
// The condition on a failed build used to be the Job's own sentence — "Job has
// reached the specified backoff limit" — which is true of every failed build
// there has ever been. The operator now reads the pod before it is collected
// and writes down which container stopped, how it exited and the last of what
// it printed, and this is where that is shown: at the top of the page, above
// the log, because it is the reason the page was opened.
const failure = computed(() => (build.value?.phase === "Failed" ? (build.value.failure ?? null) : null));

/** What the failure is called on screen. The container is the useful half —
 *  "clone" and "creator" are different diagnoses — so it leads. */
const failureTitle = computed(() => {
  const detail = failure.value;
  if (!detail) return "";
  if (detail.container && detail.exitCode !== undefined) {
    return `${detail.container} exited ${detail.exitCode}`;
  }
  if (detail.container) return `${detail.container} did not run`;
  return detail.reason || "The build failed";
});

/** The failure's own line, when it says more than the title already does. The
 *  message usually *is* the title — "creator exited 51" — and printing it
 *  twice would be the panel repeating itself; it earns its place when the
 *  kubelet had something to add. */
const failureDetail = computed(() => {
  const message = failure.value?.message ?? "";
  return message && message !== failureTitle.value ? message : "";
});

/** A running build whose Job has never created a pod, and the reason the job
 *  controller gave for it. The build is still notionally running — the Job may
 *  yet be admitted — so this is a warning rather than the failure panel, and it
 *  sits in the same place for the same reason: it is why the page was opened. */
const stall = computed(() => (build.value ? buildStallLine(build.value) : ""));

/** The condition the reconciler left, for a build that failed before it ever
 *  had a pod: a strategy the platform does not support, a commit that was
 *  refused for want of review. There is no container to name in either case. */
const failedCondition = computed(() =>
  build.value?.conditions?.find((c) => c.type === "Ready" && c.status === "False"),
);

const logFetcher = (query: LogQuery) => api.buildLogs(name.value, query);
const logStreamer = (query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
  api.streamBuildLogs(name.value, query, onLine, signal);
</script>

<template>
  <div class="space-y-6">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <template v-else-if="build">
      <PageHeader
        :title="build.git.message || build.name"
        :breadcrumb="[
          { label: 'Overview', to: '/' },
          { label: build.project, to: { name: 'project', params: { name: build.project } } },
          { label: build.name, mono: true },
        ]"
      >
        <template #badges>
          <PhaseBadge :phase="build.phase" />
        </template>
        <template #meta>
          <span class="font-mono">{{ shortSHA(build.git.sha) }}</span>
          <span class="font-mono">{{ build.git.branch }}</span>
          <span v-if="build.git.pullRequest" class="font-mono">#{{ build.git.pullRequest }}</span>
          <span v-if="build.git.author" class="font-mono">{{ build.git.author }}</span>
          <span v-if="build.detectedFramework" class="font-mono">{{ build.detectedFramework }}, detected</span>
          <!-- Which stage of a multi-stage Dockerfile this build shipped. It
               is the build's own record rather than the project's setting,
               which is the point: the setting moves and the image does not. -->
          <span v-if="build.dockerfileTarget" class="font-mono">stage {{ build.dockerfileTarget }}</span>
        </template>
        <template #actions>
          <UButton
            v-if="moving && mayCancel"
            color="neutral"
            variant="subtle"
            size="sm"
            icon="i-lucide-ban"
            :loading="cancelling"
            @click="cancel"
          >
            Cancel build
          </UButton>
        </template>
      </PageHeader>

      <div class="rounded-md border border-default bg-muted px-5 py-4 grid gap-6 sm:grid-cols-4">
        <div>
          <p class="text-xs text-muted mb-1">Created</p>
          <p class="text-sm text-toned">{{ timeAgo(build.createdAt) }}</p>
        </div>
        <div>
          <p class="text-xs text-muted mb-1">Duration</p>
          <p class="text-sm text-toned font-mono">{{ duration(build.startedAt, build.completedAt) }}</p>
          <p v-if="cacheNote" class="text-[11px] mt-0.5" :class="cacheNote.tone" :title="cacheNote.detail">
            {{ cacheNote.label }}
          </p>
        </div>
        <!-- `truncate` needs the cell to be allowed to shrink: a grid item's
             min-width is its content until min-w-0 says otherwise, and an
             image digest is wider than a phone. -->
        <div class="sm:col-span-2 min-w-0">
          <p class="text-xs text-muted mb-1">Image</p>
          <p class="text-sm text-toned font-mono truncate" :title="build.image">{{ build.image || "not pushed yet" }}</p>
        </div>
      </div>

      <!-- One commit, several images. Only for a project whose unit is more
           than one workload; the great majority ship one image and it is in
           the summary above. It sits directly under that summary because the
           question it answers — what did this commit actually produce — is the
           same question, and a build that failed on its third workload is a
           build whose first line should say which. -->
      <div v-if="build.workloads?.length" class="rounded-md border border-default px-5 py-4 space-y-3">
        <div>
          <p class="text-sm font-medium text-highlighted">
            This commit built {{ build.workloads.length + 1 }} images
          </p>
          <p class="text-xs text-muted mt-0.5">
            They ship as one thing: one release, deployed and rolled back together. The build is over when all of
            them are.
          </p>
        </div>
        <div class="overflow-x-auto">
          <table class="w-full text-left text-sm">
            <thead class="text-xs text-dimmed">
              <tr>
                <th class="py-1 pr-3 font-normal">Workload</th>
                <th class="py-1 pr-3 font-normal">Phase</th>
                <th class="py-1 font-normal">Image</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-default">
              <tr>
                <td class="py-1 pr-3 font-mono text-highlighted">web</td>
                <td class="py-1 pr-3"><PhaseBadge :phase="build.phase" /></td>
                <td class="py-1 font-mono text-toned break-all">{{ build.image || "not pushed yet" }}</td>
              </tr>
              <tr v-for="workload in build.workloads" :key="workload.name">
                <td class="py-1 pr-3 font-mono text-highlighted">{{ workload.name }}</td>
                <td class="py-1 pr-3"><PhaseBadge :phase="workload.phase" /></td>
                <td class="py-1 font-mono text-toned break-all">
                  {{ workload.image || workload.message || "not pushed yet" }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- What the commit said about itself, under the subject in the header.
           Most commits have no body and this is not rendered at all; where
           there is one it is the page's own subject matter, so it is shown
           rather than hidden behind a control. -->
      <div v-if="build.git.body" class="rounded-md border border-default px-5 py-4 space-y-2">
        <p class="text-xs text-muted">Commit message</p>
        <CommitBody :body="build.git.body" />
      </div>

      <!-- What the commit asked for itself. Above the failure panel because
           a build that failed on its own kitchen.json is a build whose first
           question is what the file said. -->
      <div v-if="build.config" class="rounded-md border border-default bg-elevated/50 px-5 py-4">
        <div class="flex items-start gap-2">
          <UIcon name="i-lucide-file-code" class="size-4 text-muted mt-0.5 shrink-0" />
          <div class="min-w-0 space-y-1">
            <p class="text-sm font-medium text-highlighted">
              This commit carried <span class="font-mono">{{ build.config.path }}</span>
            </p>
            <p v-if="build.config.declares.length" class="text-xs text-toned">
              It set
              {{ build.config.declares.length }}
              {{ build.config.declares.length === 1 ? "setting" : "settings" }}
              the project no longer decides, and the release this build produced was frozen with them.
            </p>
            <p v-else class="text-xs text-toned">It declares nothing, so every setting is the project's own.</p>
            <p v-if="build.config.declares.length" class="text-xs text-muted font-mono break-words">
              {{ build.config.declares.join(", ") }}
            </p>
          </div>
        </div>
      </div>

      <!-- Why a build that says Running is not moving. Above the failure
           panel because the two are never both showing. -->
      <div v-if="stall" class="rounded-md border border-warning/40 bg-warning/5 px-5 py-4">
        <div class="flex items-start gap-2">
          <UIcon name="i-lucide-clock-alert" class="size-4 text-warning mt-0.5 shrink-0" />
          <div class="min-w-0 space-y-1">
            <p class="text-sm font-medium text-highlighted">The build has not started</p>
            <p class="text-xs text-toned font-mono break-words">{{ stall }}</p>
          </div>
        </div>
      </div>

      <!-- Why it failed. First on the page, because on a failed build it is
           the only thing anybody came here for. -->
      <div
        v-if="build.phase === 'Failed'"
        class="rounded-md border border-error/40 bg-error/5 px-5 py-4 space-y-3"
      >
        <div class="flex items-start gap-2">
          <UIcon name="i-lucide-triangle-alert" class="size-4 text-error mt-0.5 shrink-0" />
          <div class="min-w-0 space-y-1">
            <p class="text-sm font-medium text-highlighted">
              {{ failureTitle || "The build failed" }}
              <UBadge v-if="failure?.reason" color="error" variant="subtle" size="sm" class="ml-1 font-mono">
                {{ failure.reason }}
              </UBadge>
            </p>
            <p v-if="failureDetail" class="text-xs text-toned font-mono break-words">{{ failureDetail }}</p>
            <p v-else-if="!failure && failedCondition?.message" class="text-xs text-toned break-words">
              {{ failedCondition.message }}
            </p>
            <p v-else-if="!failure && !failedCondition" class="text-xs text-muted">
              The platform recorded no detail for this failure. The build log below is what is left of it.
            </p>
          </div>
        </div>

        <!-- The last lines the failing container printed, copied onto the
             build when the failure was seen. The whole log is below and comes
             from the log store; this is what there is when the store has
             nothing — a collector that never started, or a build that failed
             before its first line was shipped. -->
        <div v-if="failure?.log?.length">
          <p class="text-[11px] text-muted mb-1">
            The last {{ failure.log.length }} lines
            <template v-if="failure.container">{{ failure.container }}</template> printed
          </p>
          <pre
            class="text-[11px] leading-relaxed font-mono text-toned bg-default/60 border border-default rounded p-3 overflow-x-auto whitespace-pre"
          >{{ failure.log.join("\n") }}</pre>
        </div>
      </div>

      <!-- The artifact, by content. An image reference is a name; this is the
           identity every claim about it is keyed to. -->
      <div v-if="build.artifact?.digest" class="rounded-md border border-default px-5 py-4 space-y-3">
        <div class="flex items-start justify-between gap-4 flex-wrap">
          <div class="min-w-0">
            <p class="text-sm font-medium text-highlighted flex items-center gap-2">
              Artifact
              <UBadge v-if="build.artifact.attested" color="success" variant="subtle" size="sm">attested</UBadge>
              <UBadge v-else color="neutral" variant="subtle" size="sm">no evidence</UBadge>
            </p>
            <p class="text-xs text-muted font-mono mt-0.5 break-all" :title="build.artifact.digest">
              {{ build.artifact.repository }}@{{ build.artifact.digest }}
            </p>
            <p v-if="build.artifact.attested" class="text-[11px] text-dimmed mt-0.5 font-mono">
              signed under {{ shortDigest("sha256:" + build.artifact.keyID) }}, {{ timeAgo(build.artifact.attestedAt!) }}
            </p>
            <p v-else-if="build.artifact.message" class="text-[11px] text-warning mt-0.5">
              {{ build.artifact.message }}
            </p>

            <!-- What is attached, without going to the registry for it. The
                 build says so itself, which is what makes this readable on a
                 build whose registry is briefly unreachable. -->
            <div v-if="build.artifact.evidence?.length" class="flex items-center gap-1.5 flex-wrap mt-2">
              <UBadge
                v-for="attached in build.artifact.evidence"
                :key="attached.predicateType"
                color="neutral"
                variant="subtle"
                size="sm"
                :title="attached.predicateType"
              >
                {{ evidenceLabel(attached.kind) }}
                <span v-if="attached.source === 'builder'" class="text-dimmed ml-1">from the builder</span>
              </UBadge>
            </div>
            <p v-if="build.artifact.message && build.artifact.attested" class="text-[11px] text-warning mt-1">
              {{ build.artifact.message }}
            </p>
          </div>
          <UButton size="xs" color="neutral" variant="subtle" :loading="loadingEvidence" @click="loadEvidence">
            {{ evidence ? "Re-read the evidence" : "Read the evidence" }}
          </UButton>
        </div>

        <UAlert
          v-if="evidenceError"
          color="error"
          variant="soft"
          icon="i-lucide-triangle-alert"
          :title="evidenceError"
        />

        <template v-else-if="evidence">
          <p v-if="!evidence.attestations.length" class="text-xs text-muted">
            Nothing is attached to this digest. The artifact is real and what is deployed from it is honest about what
            it is running — what it cannot do is satisfy a policy that requires evidence.
          </p>
          <div v-else class="space-y-2">
            <div
              v-for="found in evidence.attestations"
              :key="found.digest"
              class="rounded border border-default px-3 py-2"
            >
              <p class="text-xs flex items-center gap-2 flex-wrap">
                <UIcon
                  :name="found.verified ? 'i-lucide-shield-check' : 'i-lucide-shield-question'"
                  class="size-4"
                  :class="found.verified ? 'text-success' : 'text-dimmed'"
                />
                <span class="font-mono text-highlighted break-all">{{ predicateLabel(found.predicateType) }}</span>
                <span v-if="found.verified" class="text-success">verified</span>
                <span v-else-if="evidence.verified" class="text-warning">not signed by a key this platform holds</span>
                <span v-else class="text-dimmed">not checked</span>
              </p>
              <p class="text-[11px] text-dimmed font-mono mt-1 break-all" :title="found.digest">
                {{ shortDigest(found.digest) }}
                <template v-if="found.keyIDs?.length"> · {{ found.keyIDs.map(shortDigest).join(", ") }}</template>
              </p>
            </div>
          </div>
          <p class="text-[11px] text-dimmed leading-relaxed">
            Read straight out of the registry, attached to the digest above through OCI referrers. The same envelopes
            answer <span class="font-mono">cosign download attestation</span> with this platform out of the loop, which
            is the point of storing them there.
          </p>
        </template>
      </div>

      <!-- How the change was reviewed. This is a third party's claim and the
           panel says whose: the platform did not watch the review, it asked
           the provider and was answered. -->
      <div v-if="build.source" class="rounded-md border border-default px-5 py-4 space-y-2">
        <p class="text-sm font-medium text-highlighted flex items-center gap-2">
          Review
          <UBadge v-if="build.source.independent" color="success" variant="subtle" size="sm">
            independently approved
          </UBadge>
          <UBadge v-else-if="build.source.selfApproved" color="warning" variant="subtle" size="sm">
            self-approved
          </UBadge>
          <UBadge v-else-if="build.source.machineIdentity" color="neutral" variant="subtle" size="sm">
            machine identity
          </UBadge>
          <UBadge v-else-if="build.source.pullRequest" color="neutral" variant="subtle" size="sm">
            not approved
          </UBadge>
          <UBadge v-else color="neutral" variant="subtle" size="sm">direct push</UBadge>
        </p>

        <p v-if="build.source.pullRequest" class="text-xs text-muted">
          #{{ build.source.pullRequest }}
          <span v-if="build.source.title">— {{ build.source.title }}</span>
          <span v-if="build.source.author"> · opened by {{ build.source.author }}</span>
        </p>
        <p v-else-if="!build.source.message" class="text-xs text-muted">
          The provider associates this commit with no pull request.
        </p>

        <p v-if="build.source.approvers?.length" class="text-xs text-toned">
          approved by {{ build.source.approvers.join(", ") }}
          <span v-if="build.source.selfApproved" class="text-warning">— the author's own approval</span>
        </p>
        <p v-if="build.source.machineIdentity" class="text-xs text-toned">
          Built without review under the allowlisted identity
          <span class="font-mono">{{ build.source.machineIdentity }}</span>. The exemption is in the audit log.
        </p>
        <p v-if="build.source.message" class="text-xs text-warning">{{ build.source.message }}</p>
        <p v-if="!build.source.required" class="text-[11px] text-dimmed">
          This project does not require a reviewed pull request, so nothing here refused anything — it is recorded
          because a policy at promotion may still want it.
        </p>
        <p v-if="build.source.provider" class="text-[11px] text-dimmed">
          As reported by {{ build.source.provider }}<span v-if="build.source.checkedAt">, {{ timeAgo(build.source.checkedAt) }}</span>. The platform did not
          witness the review; it recorded what it was told, while it was still true.
        </p>
      </div>

      <!-- What the gates did. Deliberately not what they found, and
           deliberately not whether it was acceptable: a gate records facts and
           the verdict belongs to the environment being deployed to. -->
      <div v-if="build.gates?.length" class="rounded-md border border-default px-5 py-4 space-y-3">
        <div>
          <p class="text-sm font-medium text-highlighted">Quality gates</p>
          <p class="text-xs text-muted mt-0.5">
            What ran over this artifact. A gate that found problems still completed — what it found is in its
            attestation, and whether that is disqualifying is a question about the environment being deployed to.
          </p>
        </div>
        <div class="space-y-2">
          <div v-for="ran in build.gates" :key="ran.name" class="rounded border border-default px-3 py-2">
            <p class="text-xs flex items-center gap-2 flex-wrap">
              <UIcon :name="gateIcon(ran)" class="size-4" :class="gateTone(ran)" />
              <span class="font-medium text-highlighted">{{ ran.name }}</span>
              <UBadge :color="gateBadge(ran)" variant="subtle" size="sm">{{ ran.phase?.toLowerCase() }}</UBadge>
              <span v-if="ran.source === 'external'" class="text-dimmed">
                reported by {{ ran.reportedBy || "somebody else" }}
              </span>
              <span v-if="ran.attested" class="text-success">signed</span>
              <span v-else-if="ran.phase === 'Completed'" class="text-warning">not signed</span>
            </p>
            <p v-if="ran.message" class="text-[11px] text-warning mt-1">{{ ran.message }}</p>
            <p v-else-if="ran.finishedAt" class="text-[11px] text-dimmed mt-1">{{ timeAgo(ran.finishedAt) }}</p>
          </div>
        </div>
      </div>

      <!-- What has been asserted about the findings applying here. It sits
           after the gates on purpose: a gate says what was found, and this
           says what somebody claims about it — and a suppression that could
           not be seen beside the finding it suppresses would be the one thing
           this feature must not be. -->
      <VEXPanel
        v-if="build.artifact?.digest"
        :build="build.name"
        :project="build.project"
        :role="project.data.value?.role"
      />

      <OperatorOnly>
        <ConditionsTable :conditions="build.conditions" />
      </OperatorOnly>

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">Build output</h2>
        <LogViewer :fetcher="logFetcher" :streamer="logStreamer" :live="moving" :query-clause="`build = '${build.name}'`" />
      </div>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>
  </div>
</template>
