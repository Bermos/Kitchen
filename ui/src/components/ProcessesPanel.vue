<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type Process, type ProcessRun } from "../lib/api";
import { timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { useAsync, usePoll } from "../lib/useAsync";
import StatusDot from "./StatusDot.vue";

// The workers and scheduled jobs an environment runs besides its web process
// (#78).
//
// The whole point of this panel is the sentence "a CronJob whose pods fail
// silently is the classic way this feature disappoints". So a failing schedule
// is the loudest thing on it: the row is red, the failure's own message is on
// the row rather than behind a click, and it stays there until a *later
// failure* replaces it — never until a success does, because a job that fails
// four nights in five must not read as healthy on the fifth.
//
// The role is handed down rather than fetched: this panel is always on a
// screen that already loaded the project, and a second read would be the same
// answer one request later.
const props = defineProps<{ environment: string; role?: string }>();

const toast = useToast();

const caller = computed(() => callerFor(props.role));
const mayRun = computed(() => may("POST /api/v1/environments/{name}/processes/{process}/runs", caller.value));

const { data, error, refresh } = useAsync(() => api.environmentProcesses(props.environment));
watch(
  () => props.environment,
  () => void refresh(),
);

const processes = computed(() => data.value ?? []);
// A run in flight finishes on its own time, and a worker rolling out settles
// on its own time. Poll while either is true and stop when nothing moves.
const moving = computed(() =>
  processes.value.some(
    (p) => !p.suspended && ((p.active ?? 0) > 0 || (p.type === "worker" && (p.readyReplicas ?? 0) < (p.replicas ?? 0))),
  ),
);
usePoll(() => void refresh(), 5000, () => moving.value);

const expanded = ref<string | null>(null);
const runs = ref<Record<string, ProcessRun[]>>({});
const runsError = ref<Record<string, string>>({});

async function toggle(process: Process) {
  if (expanded.value === process.name) {
    expanded.value = null;
    return;
  }
  expanded.value = process.name;
  if (process.type === "cron" && !process.suspended) await loadRuns(process.name);
}

async function loadRuns(name: string) {
  try {
    runs.value = { ...runs.value, [name]: await api.processRuns(props.environment, name) };
    const { [name]: _dropped, ...rest } = runsError.value;
    runsError.value = rest;
  } catch (err) {
    runsError.value = { ...runsError.value, [name]: err instanceof Error ? err.message : String(err) };
  }
}

const starting = ref<string | null>(null);
async function runNow(process: Process) {
  starting.value = process.name;
  try {
    const started = await api.startProcessRun(props.environment, process.name);
    toast.add({
      title: `${process.name} is running`,
      description: `Run ${started.name}. Its output is in the logs under this run.`,
      color: "success",
      icon: "i-lucide-play",
    });
    await Promise.all([refresh(), loadRuns(process.name)]);
    expanded.value = process.name;
  } catch (err) {
    toast.add({
      title: `Running ${process.name} failed`,
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    starting.value = null;
  }
}

function tone(process: Process) {
  if (process.suspended) return "neutral" as const;
  return process.healthy ? ("success" as const) : ("error" as const);
}

// The one phrase the row is scanned for. A suspended process is neither well
// nor broken — it is deliberately not running — so it says that instead.
function state(process: Process): string {
  if (process.suspended) return "not run here";
  if (process.type === "cron") {
    if (!process.healthy) return "last run failed";
    if ((process.active ?? 0) > 0) return `${process.active} running`;
    return process.lastRun ? "scheduled" : "never run";
  }
  return `${process.readyReplicas ?? 0}/${process.replicas ?? 0} ready`;
}

function runTone(run: ProcessRun) {
  if (run.phase === "Failed") return "error" as const;
  return run.phase === "Running" ? ("warning" as const) : ("success" as const);
}

function took(run: ProcessRun): string {
  if (run.durationSeconds === undefined) return "still running";
  if (run.durationSeconds < 60) return `${run.durationSeconds.toFixed(1)}s`;
  return `${Math.round(run.durationSeconds / 60)}m`;
}

function commandOf(process: Process): string {
  const words = [...(process.command ?? []), ...(process.args ?? [])];
  return words.length ? words.join(" ") : "the image's own entrypoint";
}
</script>

<template>
  <div id="section-processes">
    <h2 class="text-sm font-medium text-highlighted mb-2">Workers and scheduled jobs</h2>
    <p class="text-xs text-muted mb-3">
      What this environment runs besides its web process. A worker runs continuously and has no URL; a scheduled job
      runs on its cron expression, in UTC, and every firing is a run with its own logs. The list is the release's, so
      an environment that was rolled back runs what that release declared.
    </p>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <div v-else class="rounded-md border border-default divide-y divide-default overflow-hidden">
      <p v-if="!processes.length" class="px-4 py-3 text-sm text-muted">
        No workers or scheduled jobs — this project deploys its web process alone.
      </p>
      <div v-for="process in processes" :key="process.name">
        <div
          class="flex items-center gap-3 px-4 py-2.5 text-sm cursor-pointer hover:bg-elevated"
          @click="toggle(process)"
        >
          <StatusDot :tone="tone(process)" :pulse="(process.active ?? 0) > 0" />
          <span class="font-mono text-highlighted">{{ process.name }}</span>
          <UBadge color="neutral" variant="subtle" size="sm">{{ process.type }}</UBadge>
          <span v-if="process.schedule" class="font-mono text-xs text-muted">{{ process.schedule }}</span>
          <span class="text-xs" :class="process.healthy || process.suspended ? 'text-muted' : 'text-error'">
            {{ state(process) }}
          </span>
          <span v-if="process.lastRun?.startedAt" class="ml-auto text-xs text-dimmed whitespace-nowrap">
            {{ timeAgo(process.lastRun.startedAt) }}
          </span>
          <UButton
            v-if="mayRun && process.type === 'cron' && !process.suspended"
            color="neutral"
            variant="ghost"
            size="xs"
            icon="i-lucide-play"
            :loading="starting === process.name"
            :class="process.lastRun?.startedAt ? '' : 'ml-auto'"
            aria-label="Run now"
            @click.stop="runNow(process)"
          />
        </div>

        <!-- A failure is on the row, not behind the click: the message is the
             whole reason to look at this panel, and one more interaction
             between a person and it is one more way to miss it. -->
        <div
          v-if="!process.suspended && process.lastFailure && !process.healthy"
          class="px-4 py-2 bg-error/5 border-t border-muted text-xs text-error"
        >
          {{ process.lastFailure.name }} failed {{ timeAgo(process.lastFailure.startedAt ?? "") }}
          <span v-if="process.lastFailure.message">— {{ process.lastFailure.message }}</span>
        </div>

        <div v-if="expanded === process.name" class="px-4 py-3 bg-muted space-y-3 border-t border-muted text-xs">
          <p v-if="process.suspended" class="text-muted">{{ process.reason }}</p>
          <dl class="grid grid-cols-[8rem_1fr] gap-x-4 gap-y-1">
            <dt class="text-dimmed">Command</dt>
            <dd class="font-mono text-toned break-all">{{ commandOf(process) }}</dd>
            <template v-if="process.type === 'cron'">
              <dt class="text-dimmed">Concurrency</dt>
              <dd class="text-toned">{{ process.concurrencyPolicy }}</dd>
              <dt class="text-dimmed">Timeout</dt>
              <dd class="text-toned">{{ process.timeout }}</dd>
            </template>
            <template v-if="process.cpu || process.memory">
              <dt class="text-dimmed">Resources</dt>
              <dd class="text-toned">{{ [process.cpu, process.memory].filter(Boolean).join(" / ") }}</dd>
            </template>
            <template v-if="process.workload">
              <dt class="text-dimmed">Workload</dt>
              <dd class="font-mono text-toned">{{ process.workload }}</dd>
            </template>
          </dl>

          <div v-if="process.type === 'cron' && !process.suspended">
            <p class="text-dimmed mb-1">Recent runs</p>
            <UAlert
              v-if="runsError[process.name]"
              color="error"
              variant="soft"
              icon="i-lucide-triangle-alert"
              :title="runsError[process.name]"
            />
            <p v-else-if="!runs[process.name]?.length" class="text-muted">
              No runs the platform still holds. The output of one it has collected is still in the logs, under its
              run.
            </p>
            <table v-else class="w-full text-left">
              <thead class="text-dimmed">
                <tr>
                  <th class="py-1 pr-3 font-normal">Run</th>
                  <th class="py-1 pr-3 font-normal">Phase</th>
                  <th class="py-1 pr-3 font-normal">Started</th>
                  <th class="py-1 pr-3 font-normal">Took</th>
                  <th class="py-1 font-normal">Message</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-default">
                <tr v-for="run in runs[process.name]" :key="run.name">
                  <td class="py-1.5 pr-3 font-mono text-toned break-all">{{ run.name }}</td>
                  <td class="py-1.5 pr-3">
                    <span class="inline-flex items-center gap-1.5">
                      <StatusDot :tone="runTone(run)" :pulse="run.phase === 'Running'" />
                      {{ run.phase }}
                    </span>
                  </td>
                  <td class="py-1.5 pr-3 text-muted">{{ run.startedAt ? timeAgo(run.startedAt) : "—" }}</td>
                  <td class="py-1.5 pr-3 text-muted">{{ took(run) }}</td>
                  <td class="py-1.5 text-error">{{ run.message }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
