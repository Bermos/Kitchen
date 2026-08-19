<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { api, type EvidenceSet, type LogLine, type LogQuery } from "../lib/api";
import { duration, shortSHA, timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { operatorMode } from "../lib/mode";
import { may } from "../lib/policy";
import { useAsync, usePoll } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";
import LogViewer from "../components/LogViewer.vue";
import PhaseBadge from "../components/PhaseBadge.vue";

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

const logFetcher = (query: LogQuery) => api.buildLogs(name.value, query);
const logStreamer = (query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
  api.streamBuildLogs(name.value, query, onLine, signal);
</script>

<template>
  <div class="space-y-6">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <template v-else-if="build">
      <div>
        <div class="flex items-center gap-2 text-xs text-muted mb-1">
          <RouterLink to="/" class="hover:text-highlighted">Overview</RouterLink>
          <span>/</span>
          <RouterLink :to="{ name: 'project', params: { name: build.project } }" class="hover:text-highlighted">
            {{ build.project }}
          </RouterLink>
          <span>/</span>
          <span class="text-toned font-mono">{{ build.name }}</span>
        </div>
        <div class="flex items-center gap-3 flex-wrap">
          <h1 class="text-xl font-semibold text-highlighted">{{ build.git.message || build.name }}</h1>
          <PhaseBadge :phase="build.phase" />
          <span class="flex-1" />
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
        </div>
        <div class="flex items-center gap-3 mt-1 text-xs text-muted font-mono flex-wrap">
          <span>{{ shortSHA(build.git.sha) }}</span>
          <span>{{ build.git.branch }}</span>
          <span v-if="build.git.pullRequest">#{{ build.git.pullRequest }}</span>
          <span v-if="build.git.author">{{ build.git.author }}</span>
          <span v-if="build.detectedFramework">{{ build.detectedFramework }}, detected</span>
        </div>
      </div>

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

      <ConditionsTable v-if="operatorMode" :conditions="build.conditions" />

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">Build output</h2>
        <LogViewer :fetcher="logFetcher" :streamer="logStreamer" :live="moving" :query-clause="`build = '${build.name}'`" />
      </div>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>
  </div>
</template>
