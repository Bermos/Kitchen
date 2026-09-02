<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type Process, type ProcessRun } from "../lib/api";
import { timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { useAsync, usePoll } from "../lib/useAsync";
import StatusDot from "./StatusDot.vue";

// The workloads an environment runs besides its web process (#78, #271): its
// workers, its scheduled jobs, and the services the rest of the unit talks to
// over the cluster network.
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
    (p) =>
      !p.suspended &&
      ((p.active ?? 0) > 0 || (p.type !== "cron" && (p.readyReplicas ?? 0) < (p.replicas ?? 0))),
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

// The variable a sibling of this service reads to find it. It is the one
// thing about a service nothing else on the row says, and the thing somebody
// wiring two workloads together is looking for.
function serviceVariable(process: Process): string {
  return `KITCHEN_SERVICE_${process.name.toUpperCase().replaceAll("-", "_")}`;
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

// A workload's health check in one line: what is asked, where, and how often.
// A worker is probed only where it asked to be, so there is nothing to show
// for one that declared none.
function healthOf(process: Process): string {
  const health = process.health;
  if (!health) return "";
  const target = health.path ? `GET ${health.path} on :${health.port}` : `TCP :${health.port}`;
  return `${target} every ${health.periodSeconds}s, ${health.failureThreshold} failures out`;
}

// Where a workload's image comes from: its own directory of the repository,
// or the project's own image run with another command. It is the whole of
// what makes a monorepo one project — four directories, four images, one
// commit — so it is on the row's detail rather than left to be inferred from
// an image reference nobody reads.
function buildOf(process: Process): string {
  const build = process.build;
  if (!build) return "the project's own image, started differently";
  const where = build.rootDirectory && build.rootDirectory !== "." ? build.rootDirectory : "the repository root";
  if (build.strategy === "dockerfile") {
    // Which stage of that file, when this workload names one. A workload that
    // names none is built to the project's stage, which the build's own page
    // reports per image — saying "the last stage" here would be a guess.
    const stage = build.dockerfileTarget ? `, stage ${build.dockerfileTarget}` : "";
    return `${build.dockerfilePath ?? "Dockerfile"} in ${where}${stage}`;
  }
  return `buildpacks, from ${where}`;
}
</script>

<template>
  <div id="section-processes">
    <h2 class="text-sm font-medium text-highlighted mb-2">Workloads</h2>
    <p class="text-xs text-muted mb-3">
      What this environment runs besides its web process. A worker runs continuously and is never addressed; a
      service runs continuously and is reachable from the rest of this environment, and is never published; a
      scheduled job runs on its cron expression, in UTC, and every firing is a run with its own logs. The list is
      the release's, so an environment that was rolled back runs what that release declared — the same workloads,
      built to the same images.
    </p>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <div v-else class="rounded-md border border-default divide-y divide-default overflow-hidden">
      <p v-if="!processes.length" class="px-4 py-3 text-sm text-muted">
        No other workloads — this project deploys its web process alone.
      </p>
      <div v-for="process in processes" :key="process.name">
        <div
          class="flex items-center gap-3 px-4 py-2.5 text-sm cursor-pointer hover:bg-elevated"
          @click="toggle(process)"
        >
          <StatusDot :tone="tone(process)" :pulse="(process.active ?? 0) > 0" />
          <span class="font-mono text-highlighted">{{ process.name }}</span>
          <UBadge color="neutral" variant="subtle" size="sm">{{ process.type }}</UBadge>
          <!-- A worker that must never run twice deploys differently from
               every other row here, and the replica count does not say so:
               1/1 ready reads the same either way. -->
          <UBadge v-if="process.singleton" color="neutral" variant="outline" size="sm">one at a time</UBadge>
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
            <dt class="text-dimmed">Built from</dt>
            <dd class="text-toned break-all">{{ buildOf(process) }}</dd>
            <template v-if="process.address">
              <dt class="text-dimmed">Address</dt>
              <dd class="text-toned break-all">
                <span class="font-mono">{{ process.address }}</span>
                <span class="block text-dimmed mt-0.5">
                  This environment's other workloads reach it here, reading it as
                  <span class="font-mono">{{ serviceVariable(process) }}</span> with the host and the port beside it
                  under the same name. Nothing else can: a service is never published, and has no URL.
                </span>
              </dd>
            </template>
            <template v-else-if="process.port">
              <dt class="text-dimmed">Port</dt>
              <dd class="font-mono text-toned">{{ process.port }}</dd>
            </template>
            <template v-if="process.singleton">
              <dt class="text-dimmed">Deploys</dt>
              <dd class="text-toned">
                One copy at a time — the old copy stops before the new one starts, so a deploy never overlaps two of
                it. That is a few seconds of this worker not consuming, which is the trade a poller or an ingest
                loop asked for.
              </dd>
            </template>
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
            <template v-if="process.health">
              <dt class="text-dimmed">Health</dt>
              <dd class="text-toned font-mono break-all">{{ healthOf(process) }}</dd>
            </template>
            <template v-if="process.image">
              <dt class="text-dimmed">Image</dt>
              <dd class="font-mono text-toned break-all">{{ process.image }}</dd>
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
                  <td class="py-1 pr-3 font-mono text-toned break-all">{{ run.name }}</td>
                  <td class="py-1 pr-3">
                    <span class="inline-flex items-center gap-1.5">
                      <StatusDot :tone="runTone(run)" :pulse="run.phase === 'Running'" />
                      {{ run.phase }}
                    </span>
                  </td>
                  <td class="py-1 pr-3 text-muted">{{ run.startedAt ? timeAgo(run.startedAt) : "—" }}</td>
                  <td class="py-1 pr-3 text-muted">{{ took(run) }}</td>
                  <td class="py-1 text-error">{{ run.message }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
