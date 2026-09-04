<script setup lang="ts">
import { computed } from "vue";
import type { Artifact } from "../lib/api";

/** What one image of a unit carries, at a glance.
 *
 *  A commit produces one image per workload that declares a build of its own,
 *  and every one of them is deployed by the release — so "attested" is a fact
 *  about an image, not about a commit, and a row that showed the project's own
 *  answer for all of them would be a compliance surface reporting success over
 *  images nothing looked at.
 *
 *  It is the index and not the evidence: what the build says is attached,
 *  readable without going to the registry for it. The evidence itself is
 *  behind the panel's own read. */
const props = defineProps<{ artifact?: Artifact }>();

/** What to call each kind of evidence on screen. The API sends the label
 *  alongside the URI, so this maps labels rather than parsing URIs. */
const labels: Record<string, string> = {
  provenance: "provenance",
  sbom: "SBOM",
  buildRecord: "build record",
  deployment: "deployment",
  other: "other",
};

function label(kind: string): string {
  return labels[kind] ?? labels.other;
}

/** Whether this is an image somebody else built. It is shown before the
 *  evidence chips rather than among them, because it is not evidence: it is
 *  what kind of evidence the rest of the row can be. */
const vendored = computed(() => props.artifact?.sourceType === "vendored");

/** One chip per kind, rather than one per attestation: a gate that ran four
 *  times is four attestations and one answer to "what is attached". */
const kinds = computed(() => {
  const seen: string[] = [];
  for (const attached of props.artifact?.evidence ?? []) {
    const name = label(attached.kind);
    if (!seen.includes(name)) seen.push(name);
  }
  return seen;
});
</script>

<template>
  <div v-if="artifact?.digest" class="flex items-center gap-1.5 flex-wrap">
    <UBadge
      v-if="vendored"
      color="info"
      variant="subtle"
      size="sm"
      :title="artifact.upstream?.reference ? 'from ' + artifact.upstream.reference : 'built by somebody else'"
      >vendored</UBadge
    >
    <UBadge v-if="artifact.attested" color="success" variant="subtle" size="sm">attested</UBadge>
    <UBadge v-else color="neutral" variant="subtle" size="sm">no evidence</UBadge>
    <UBadge v-for="kind in kinds" :key="kind" color="neutral" variant="subtle" size="sm">{{ kind }}</UBadge>
  </div>
  <span v-else class="text-xs text-dimmed">—</span>
</template>
