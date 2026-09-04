<script setup lang="ts">
import { newVolumeInitDraft, type VolumeInitDraft } from "../lib/workloads";

// What a workload prepares inside the volumes it mounts, before it starts
// (#348).
//
// One editor, used twice: the web process's on the project's settings form and
// a named workload's inside the workloads editor. They are the same
// declaration one level apart — a volume claim names the one process that
// mounts it, and the process that mounts an empty filesystem is the one that
// cannot start on it — so a second copy of this form would be a second place
// for the two to drift.
//
// Nothing here is operator vocabulary. A volume is named by the claim the
// project made; the PersistentVolumeClaim behind it is the claims screen's,
// and behind `<OperatorOnly>` there.

const drafts = defineModel<VolumeInitDraft[]>({ required: true });

const props = defineProps<{
  /** Off for a reader, and for a project whose repository declares this. */
  mayEdit: boolean;
  /** Where it sits in the page: a section of the settings screen for the web
   * process, a block inside one workload's card in the workloads editor. The
   * two tags are written out rather than computed so that the heading scale in
   * docs/UI.md is checked here like anywhere else. */
  heading: "h2" | "h3";
}>();

function add() {
  drafts.value = [...drafts.value, newVolumeInitDraft()];
}

function remove(index: number) {
  drafts.value = drafts.value.filter((_, at) => at !== index);
}
</script>

<template>
  <div class="space-y-3">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 v-if="props.heading === 'h2'" class="text-sm font-medium text-highlighted">Before it starts</h2>
        <h3 v-else class="text-xs font-medium text-highlighted">Before it starts</h3>
        <p class="text-xs text-muted mt-1">
          A volume arrives empty, and plenty of software will not start on one: it wants its directories to exist, and
          sometimes a configuration file it can then rewrite for itself. This is what the platform does inside a volume
          before the workload's own process runs — as the user the workload runs as, and only where something is
          missing, so a later deploy never overwrites what the application has written.
        </p>
      </div>
      <UButton
        v-if="props.mayEdit"
        color="neutral"
        variant="subtle"
        size="xs"
        icon="i-lucide-plus"
        @click="add"
      >
        Prepare a volume
      </UButton>
    </div>

    <p v-if="!drafts.length" class="text-xs text-dimmed">
      Nothing is prepared. Most workloads need nothing here — it is for software that cannot start on an empty
      filesystem.
    </p>

    <div v-for="(draft, index) in drafts" :key="draft.key" class="rounded-md border border-default bg-default p-4 space-y-4">
      <div class="flex items-start gap-2">
        <UFormField
          label="Volume"
          class="flex-1"
          help="One of the volumes this workload mounts, by the name of the claim."
        >
          <UInput v-model="draft.volume" :disabled="!props.mayEdit" class="w-full font-mono" placeholder="config" />
        </UFormField>
        <UButton
          v-if="props.mayEdit"
          color="neutral"
          variant="ghost"
          size="xs"
          icon="i-lucide-x"
          aria-label="Stop preparing this volume"
          class="mt-6"
          @click="remove(index)"
        />
      </div>

      <UFormField
        label="Directories"
        help="One per line, inside the volume: a path, and optionally the permissions it is created with — custom_components 0750. A directory that is already there is left exactly as it is."
      >
        <UTextarea
          v-model="draft.directories"
          :disabled="!props.mayEdit"
          :rows="3"
          class="w-full font-mono"
          placeholder="custom_components&#10;secrets 0700"
        />
      </UFormField>

      <UFormField
        label="Files to seed"
        help="One per line: the name of one of this project's files, where it goes inside the volume, and optionally its permissions — configuration configuration.yaml. It is copied in only where nothing is there, so the application owns it from then on."
      >
        <UTextarea
          v-model="draft.seed"
          :disabled="!props.mayEdit"
          :rows="3"
          class="w-full font-mono"
          placeholder="configuration configuration.yaml"
        />
      </UFormField>
    </div>
  </div>
</template>
