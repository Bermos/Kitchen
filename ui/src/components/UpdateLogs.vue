<script setup lang="ts">
import { api, type LogLine, type LogQuery } from "../lib/api";
import LogViewer from "./LogViewer.vue";

// helm's own output for one platform upgrade, through the same viewer a build's
// output uses: a bounded page, followed as Server-Sent Events while the upgrade
// is still running, and stopped the moment `live` goes false or this panel is
// unmounted.
//
// What it is for is worth being plain about. `helm upgrade --atomic --wait`
// says almost nothing between "beginning upgrade" and its verdict, so this is
// not where progress is watched — that is the component checklist, which moves
// while helm is silent. This is where a *failure* explains itself: the release
// that could not be rendered, the object that would not become ready, the
// rollback that followed.

const props = defineProps<{
  /** The PlatformUpdate's name. */
  name: string;
  /** Follow the output — an upgrade that has not finished. */
  live?: boolean;
}>();

const fetcher = (query: LogQuery) => api.updateLogs(props.name, query);
const streamer = (query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
  api.streamUpdateLogs(props.name, query, onLine, signal);
</script>

<template>
  <div class="space-y-2">
    <LogViewer :fetcher="fetcher" :streamer="streamer" :live="live" />
    <p class="text-[11px] text-dimmed leading-relaxed">
      <span class="font-mono">helm upgrade --atomic --wait</span> prints very little between starting and finishing, so
      an empty panel here is the normal case rather than a lost log. It is where a failed upgrade says what it could not
      apply and what it rolled back to; the progress is the checklist above. The job is reaped an hour after it
      finishes and these lines outlive it, so an upgrade from last month reads the same as this one.
    </p>
  </div>
</template>
