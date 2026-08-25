<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type Build, type Environment, type Release, type RequestSummary, type VariableChange } from "../lib/api";
import { compactCount, shortImage, shortSHA, timeAgo } from "../lib/format";
import {
  changeDetail,
  changeSign,
  commitsBetween,
  gatedByName,
  lastServedStint,
  movedProcesses,
  movedRuntime,
  movedVariables,
  releaseRows,
  servingSince,
  swapLanded,
  unchangedVariables,
  variableCounts,
  verificationChecks,
  type ReleaseRow,
} from "../lib/rollback";
import { useAsync, usePoll } from "../lib/useAsync";

// Rollback, in three explicit steps: pick, review the diff, verify (#181).
//
// **The middle step is the whole point.** Rollback is the one destructive
// action the dashboard offers, usually taken under pressure by whoever is on
// call rather than by whoever wrote the code — and until this panel existed
// the confirm dialog revealed nothing the release list had not already shown.
// Everything it needed was already in the API; it just was not on screen at
// the moment of the decision. So step two puts it there: live against target,
// the image digest that will actually be pulled, the variable-level diff, and
// the commits that stop being served.
//
// **The third step is the other half of the same complaint.** A panel that
// closed on confirm would leave "did that work" to be answered by going and
// looking somewhere else, so it stays and answers it: replicas rolled, route
// programmed, 5xx since the swap, p95 measured against the window before it.
//
// No value is ever shown, because none is ever fetched. The diff comes from
// `GET /releases/{name}/config-diff`, which compares the two snapshots on the
// server for exactly that reason — see internal/api/configdiff.go.

const props = defineProps<{
  open: boolean;
  environment: Environment;
  releases: Release[];
  builds: Build[];
  /** The release the panel opens on, when it was opened from a row rather
   * than from the header. The rail is still there to change it. */
  initialRelease?: string;
}>();
const emit = defineEmits<{ "update:open": [boolean]; moved: [] }>();

const toast = useToast();

const rows = computed(() => releaseRows(props.releases, props.builds, props.environment));
const liveRow = computed(() => rows.value.find((row) => row.live));
const target = ref<ReleaseRow | null>(null);

// Step three begins when the write is accepted, not when it lands: the
// operator applies the patch asynchronously, and watching it land *is* the
// verification.
const swappedAt = ref<string>("");
// What was live when the swap was made. Read off the environment it would be
// wrong within a second: the parent refreshes, and the release the panel is
// telling somebody is still in the registry would be the one it just made
// live.
const swappedFrom = ref<string>("");
// The environment's name, typed out, when the move is one that asks for it.
// Declared here rather than beside the gate it belongs to, because the watcher
// below clears it and runs while the panel is being set up.
const typed = ref("");
const step = computed(() => (swappedAt.value ? 3 : target.value ? 2 : 1));
const stepLabel = computed(() =>
  step.value === 3 ? "Step 3 of 3 — verifying" : step.value === 2 ? "Step 2 of 3 — review the diff" : "Step 1 of 3 — pick a release",
);

// A move to a release newer than the live one is a promotion, not a rollback,
// and the panel says so rather than pretending otherwise.
const promoting = computed(() => (target.value?.distance ?? 0) < 0);

function close(open: boolean) {
  if (open) return;
  emit("update:open", false);
}
watch(
  () => props.open,
  (open) => {
    if (open) {
      // Opened from a row: land on that release's diff rather than making
      // somebody pick the one they just clicked.
      target.value = rows.value.find((row) => row.release.name === props.initialRelease && !row.live) ?? null;
      return;
    }
    // A closed panel forgets where it was. Reopening it mid-verification
    // would show checks about a swap somebody has stopped watching.
    target.value = null;
    swappedAt.value = "";
    swappedFrom.value = "";
    typed.value = "";
  },
  { immediate: true },
);

// ── Step 2: the diff ───────────────────────────────────────────────────────
//
// Fetched per target, and cleared the moment the target changes so that the
// panel never shows one release's diff under another's name.
const diff = useAsync(
  async () => {
    if (!target.value) return null;
    return api.releaseConfigDiff(target.value.release.name, props.environment.release);
  },
  { immediate: false },
);
// What each side actually served, over the window it served it for. This is
// the "last green" claim made honestly: not that somebody labelled the release
// good, but that when it last ran it answered without a 5xx. Two summaries,
// each over its own window, and neither of them costs anything until a release
// is picked.
const servedRecord = useAsync(
  async () => {
    if (!target.value) return null;
    const stint = lastServedStint(props.environment, target.value.release.name);
    const [live, previously] = await Promise.all([
      api.requestSummary(props.environment.name, { since: servingSince(props.environment) }),
      stint ? api.requestSummary(props.environment.name, { since: stint.from, until: stint.to }) : Promise.resolve(null),
    ]);
    return { live, previously };
  },
  { immediate: false },
);

watch(
  target,
  () => {
    diff.data.value = null;
    diff.error.value = null;
    servedRecord.data.value = null;
    if (!target.value) return;
    void diff.refresh();
    void servedRecord.refresh();
  },
  // Immediate, because a panel opened from a row already has its target by
  // the time this registers.
  { immediate: true },
);

/** "214 requests · 0 5xx", or nothing at all where the edge has not answered.
 *  An environment nobody has asked for anything says so rather than claiming
 *  a clean record it has not earned. */
function servedText(summary: RequestSummary | null | undefined): string {
  if (!summary) return "";
  if (summary.requests === 0) return "no requests";
  return `${compactCount(summary.requests)} requests · ${summary.errors} 5xx`;
}
const liveRecord = computed(() => servedText(servedRecord.data.value?.live));
const servedRecordText = computed(() => servedText(servedRecord.data.value?.previously));

const counts = computed(() => variableCounts(diff.data.value ?? undefined));
const moved = computed(() => movedVariables(diff.data.value ?? undefined));
const unchanged = computed(() => unchangedVariables(diff.data.value ?? undefined));
const runtimeChanges = computed(() => movedRuntime(diff.data.value ?? undefined));
const processChanges = computed(() => movedProcesses(diff.data.value ?? undefined));
const commits = computed(() => commitsBetween(rows.value, target.value?.release));

function sourceLabel(variable: VariableChange): string {
  const source = variable.source ?? variable.againstSource;
  if (source === "claim") return "Claim";
  if (source === "secret") return "Secret";
  return "Value";
}

const stint = computed(() => (target.value ? lastServedStint(props.environment, target.value.release.name) : undefined));

// ── The gate ───────────────────────────────────────────────────────────────
const gated = computed(() => gatedByName(props.environment, target.value ?? undefined, diff.data.value ?? undefined));
const confirmable = computed(() => !gated.value || typed.value.trim() === props.environment.name);

// ── The write ──────────────────────────────────────────────────────────────
const moving = ref(false);
async function move() {
  if (!target.value || !confirmable.value) return;
  const to = target.value.release.name;
  const from = props.environment.release;
  moving.value = true;
  try {
    const outcome = await api.moveEnvironment(props.environment.name, to);
    // An environment that declares requirements answers with the promotion
    // the move became: nothing has moved, so there is nothing to verify and
    // the panel says where the verdict will land instead of staying to watch
    // a swap that has not been agreed to.
    if ("trigger" in outcome) {
      toast.add({
        title: `Promotion ${outcome.name} requested`,
        description: "This environment declares requirements; the policy decides whether the release lands.",
        color: "info",
        icon: "i-lucide-scale",
      });
      emit("moved");
      emit("update:open", false);
      return;
    }
    swappedFrom.value = from;
    swappedAt.value = new Date().toISOString();
    emit("moved");
  } catch (err) {
    toast.add({
      title: "Move failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    moving.value = false;
  }
}

// ── Step 3: verification ───────────────────────────────────────────────────
//
// Everything here is read fresh and on a timer while the panel is open: the
// screen behind it polls too, but a verification that inherited a payload
// fetched before the swap would be answering about the release it replaced.
const landed = computed(() => swappedAt.value !== "" && swapLanded(props.environment, target.value?.release.name ?? ""));
const verifying = computed(() => props.open && step.value === 3);

const workload = useAsync(() => api.environmentWorkload(props.environment.name), { immediate: false });
// The window since the swap, and the same length of window before it — which
// is what makes "still settling" a comparison rather than a number nobody can
// place. Both are re-read on every tick, because the first only grows.
const since = useAsync(
  async () => (swappedAt.value ? api.requestSummary(props.environment.name, { since: swappedAt.value }) : null),
  { immediate: false },
);
const baseline = useAsync(
  async () => {
    if (!swappedAt.value) return null;
    const swap = new Date(swappedAt.value);
    const elapsed = Math.max(Date.now() - swap.getTime(), 60_000);
    return api.requestSummary(props.environment.name, {
      since: new Date(swap.getTime() - elapsed).toISOString(),
      until: swappedAt.value,
    });
  },
  { immediate: false },
);

watch(verifying, (on) => {
  if (!on) return;
  void workload.refresh();
  void since.refresh();
  void baseline.refresh();
});
usePoll(
  () => {
    void workload.refresh();
    void since.refresh();
    void baseline.refresh();
  },
  5000,
  () => verifying.value,
);

const checks = computed(() =>
  verificationChecks({
    environment: props.environment,
    target: target.value?.release.name ?? "",
    workload: workload.data.value ?? undefined,
    since: since.data.value ?? undefined,
    baseline: baseline.data.value ?? undefined,
  }),
);

function checkColor(state: string): string {
  if (state === "ok") return "bg-success";
  if (state === "bad") return "bg-error";
  return "bg-warning";
}

function commitLabel(build: Build): string {
  return build.git?.message || "no commit message";
}
</script>

<template>
  <UModal
    :open="props.open"
    fullscreen
    :title="`Move ${props.environment.name}`"
    :description="stepLabel"
    @update:open="close"
  >
    <template #content>
      <div class="flex flex-col h-full">
        <!-- Where you are, and how far through. -->
        <div class="flex items-center justify-between gap-4 border-b border-default px-5 py-3">
          <div class="flex items-center gap-2 text-xs">
            <span class="font-mono text-muted">{{ props.environment.project }}</span>
            <span class="text-dimmed">/</span>
            <span class="font-mono text-highlighted">{{ props.environment.name }}</span>
            <UBadge color="success" variant="subtle" size="sm">{{ props.environment.phase || "Live" }}</UBadge>
          </div>
          <div class="flex items-center gap-3">
            <span class="text-xs text-muted">{{ stepLabel }}</span>
            <UButton
              color="neutral"
              variant="ghost"
              size="xs"
              icon="i-lucide-x"
              aria-label="Close"
              @click="close(false)"
            />
          </div>
        </div>

        <div class="flex-1 min-h-0 grid md:grid-cols-[20rem_1fr]">
          <!-- The rail: every release of the project, annotated against this
               environment. It stays through all three steps, so what was
               picked is still on screen while the swap is being watched. -->
          <div class="border-b md:border-b-0 md:border-r border-default overflow-y-auto">
            <p class="px-4 pt-4 pb-2 text-[0.7rem] uppercase tracking-wide text-muted">Move to</p>
            <button
              v-for="row in rows"
              :key="row.release.name"
              type="button"
              class="w-full text-left px-4 py-3 border-b border-muted last:border-0 flex items-start gap-3 hover:bg-elevated disabled:hover:bg-transparent disabled:opacity-60 disabled:cursor-default"
              :class="target?.release.name === row.release.name ? 'bg-elevated' : ''"
              :disabled="row.live || step === 3"
              @click="target = row"
            >
              <span
                class="mt-1.5 size-2.5 rounded-full shrink-0"
                :class="
                  row.live
                    ? 'bg-success'
                    : target?.release.name === row.release.name
                      ? 'bg-primary'
                      : 'bg-transparent ring-1 ring-inset ring-accented'
                "
              />
              <span class="min-w-0 flex-1">
                <span class="flex items-baseline gap-2 flex-wrap">
                  <span class="font-mono text-sm text-highlighted">{{ row.release.name }}</span>
                  <span v-if="row.live" class="text-xs text-success">live</span>
                  <span v-else-if="row.lastServed" class="text-xs text-primary">last served</span>
                  <span v-else-if="row.rolledBackBefore" class="text-xs text-warning">rolled back once</span>
                </span>
                <span class="block text-xs text-muted truncate mt-0.5">
                  {{ row.build ? commitLabel(row.build) : "the build has been collected" }}
                </span>
              </span>
              <span class="text-xs text-dimmed whitespace-nowrap">{{ timeAgo(row.release.createdAt) }}</span>
            </button>
          </div>

          <!-- The pane: nothing until a release is picked, the diff once one
               is, the verification once the write is made. -->
          <div class="overflow-y-auto p-5">
            <p v-if="step === 1" class="text-sm text-muted max-w-prose">
              Pick a release. The next step shows what moving there would change — the image, the environment
              variables, and the commits that stop being served — before anything is written.
            </p>

            <template v-else-if="step === 2 && target">
              <div class="flex items-center gap-3 flex-wrap mb-4">
                <h2 class="text-base font-medium text-highlighted font-mono">
                  {{ props.environment.release }} → {{ target.release.name }}
                </h2>
                <UBadge v-if="props.environment.type !== 'preview'" color="warning" variant="subtle" size="sm">
                  production write
                </UBadge>
                <UBadge v-if="promoting" color="neutral" variant="subtle" size="sm">forward — a promotion</UBadge>
              </div>

              <!-- Live against target, side by side: the release, the digest
                   that will actually be pulled, what it was built from, and
                   what each has served. -->
              <div class="grid gap-4 sm:grid-cols-2 mb-6">
                <div class="rounded-md border border-default bg-muted px-4 py-3">
                  <p class="text-[0.7rem] uppercase tracking-wide text-muted mb-2">Live now</p>
                  <dl class="space-y-1.5 text-sm">
                    <div class="flex gap-3">
                      <dt class="text-muted w-24 shrink-0">Release</dt>
                      <dd class="font-mono text-highlighted truncate">{{ props.environment.release }}</dd>
                    </div>
                    <div class="flex gap-3">
                      <dt class="text-muted w-24 shrink-0">Image</dt>
                      <dd class="font-mono text-toned truncate" :title="liveRow?.release.image">
                        {{ shortImage(liveRow?.release.image) }}
                      </dd>
                    </div>
                    <div class="flex gap-3">
                      <dt class="text-muted w-24 shrink-0">Built from</dt>
                      <dd class="font-mono text-toned truncate">
                        {{ liveRow?.build ? `${shortSHA(liveRow.build.git.sha)} · ${liveRow.build.git.branch}` : "—" }}
                      </dd>
                    </div>
                    <div class="flex gap-3">
                      <dt class="text-muted w-24 shrink-0">Serving since</dt>
                      <dd class="text-toned">
                        {{ timeAgo(servingSince(props.environment)) }}{{ liveRecord ? ` · ${liveRecord}` : "" }}
                      </dd>
                    </div>
                  </dl>
                </div>
                <div class="rounded-md border border-primary/40 bg-primary/5 px-4 py-3">
                  <p class="text-[0.7rem] uppercase tracking-wide text-primary mb-2">
                    {{ promoting ? "After the promotion" : "After the rollback" }}
                  </p>
                  <dl class="space-y-1.5 text-sm">
                    <div class="flex gap-3">
                      <dt class="text-muted w-24 shrink-0">Release</dt>
                      <dd class="font-mono text-highlighted truncate">{{ target.release.name }}</dd>
                    </div>
                    <div class="flex gap-3">
                      <dt class="text-muted w-24 shrink-0">Image</dt>
                      <dd class="font-mono text-primary truncate" :title="target.release.image">
                        {{ shortImage(target.release.image) }}
                      </dd>
                    </div>
                    <div class="flex gap-3">
                      <dt class="text-muted w-24 shrink-0">Built from</dt>
                      <dd class="font-mono text-toned truncate">
                        {{ target.build ? `${shortSHA(target.build.git.sha)} · ${target.build.git.branch}` : "—" }}
                      </dd>
                    </div>
                    <div class="flex gap-3">
                      <dt class="text-muted w-24 shrink-0">Last served</dt>
                      <dd class="text-toned">
                        {{ stint ? `until ${timeAgo(stint.to)}` : "never on this environment"
                        }}{{ servedRecordText ? ` · ${servedRecordText}` : "" }}
                      </dd>
                    </div>
                  </dl>
                </div>
              </div>

              <!-- The variable diff. The values are not here and cannot be:
                   the API never reads one back, so the comparison is the
                   server's and only its verdict crosses the wire. -->
              <div class="mb-6">
                <div class="flex items-baseline justify-between gap-3 mb-2">
                  <p class="text-[0.7rem] uppercase tracking-wide text-muted">Environment variables in the snapshot</p>
                  <p class="text-xs text-muted">
                    {{ counts.changed }} changed · {{ counts.removed }} removed · {{ counts.added }} added
                  </p>
                </div>
                <UAlert
                  v-if="diff.error.value"
                  color="error"
                  variant="soft"
                  icon="i-lucide-triangle-alert"
                  :title="diff.error.value"
                  description="Without the diff this move is the confirm dialog it used to be — read the release's build before writing."
                />
                <p v-else-if="diff.loading.value && !diff.data.value" class="text-sm text-muted">Comparing snapshots…</p>
                <div v-else class="rounded-md border border-default overflow-hidden">
                  <div
                    v-for="variable in moved"
                    :key="variable.name"
                    class="flex items-center gap-4 px-4 py-2.5 border-b border-muted last:border-0 text-sm"
                    :class="variable.change === 'removed' ? 'bg-error/5' : 'bg-warning/5'"
                  >
                    <span class="font-mono w-4 shrink-0 text-center text-toned">{{ changeSign(variable.change) }}</span>
                    <span class="font-mono text-highlighted flex-1 min-w-0 truncate">{{ variable.name }}</span>
                    <span class="text-xs text-muted flex-1 min-w-0 truncate">{{ changeDetail(variable) }}</span>
                    <span class="text-xs text-dimmed shrink-0">{{ sourceLabel(variable) }}</span>
                  </div>
                  <!-- The unchanged ones are one row, not forty: a list of
                       names nobody has to check buries the ones they do. -->
                  <div v-if="unchanged.length" class="flex items-center gap-4 px-4 py-2.5 text-sm">
                    <span class="font-mono w-4 shrink-0 text-center text-dimmed">=</span>
                    <span class="font-mono text-toned flex-1 min-w-0 truncate" :title="unchanged.map((v) => v.name).join(', ')">
                      {{ unchanged.map((v) => v.name).join(", ") }}
                    </span>
                    <span class="text-xs text-muted shrink-0">{{ unchanged.length }} unchanged</span>
                  </div>
                  <p v-if="!moved.length && !unchanged.length" class="px-4 py-3 text-sm text-muted">
                    Neither release carries any environment variables.
                  </p>
                </div>
              </div>

              <!-- The rest of the snapshot. A rollback that quietly restored
                   yesterday's replica count or last week's cron schedule is
                   the surprise this whole panel exists to prevent. -->
              <div v-if="runtimeChanges.length || processChanges.length" class="mb-6">
                <p class="text-[0.7rem] uppercase tracking-wide text-muted mb-2">The rest of the snapshot</p>
                <div class="rounded-md border border-default overflow-hidden text-sm">
                  <div
                    v-for="field in runtimeChanges"
                    :key="field.field"
                    class="flex items-center gap-4 px-4 py-2.5 border-b border-muted last:border-0"
                  >
                    <span class="font-mono w-4 shrink-0 text-center text-toned">~</span>
                    <span class="font-mono text-highlighted flex-1">{{ field.field }}</span>
                    <span class="text-xs text-muted flex-1 truncate">
                      {{ field.from || "unset" }} → {{ field.to || "unset" }}
                    </span>
                    <span class="text-xs text-dimmed shrink-0">Runtime</span>
                  </div>
                  <div
                    v-for="process in processChanges"
                    :key="process.name"
                    class="flex items-center gap-4 px-4 py-2.5 border-b border-muted last:border-0"
                  >
                    <span class="font-mono w-4 shrink-0 text-center text-toned">{{ changeSign(process.change) }}</span>
                    <span class="font-mono text-highlighted flex-1">{{ process.name }}</span>
                    <span class="text-xs text-muted flex-1 truncate">
                      {{ process.change }}{{ process.schedule ? ` · ${process.schedule}` : "" }}
                    </span>
                    <span class="text-xs text-dimmed shrink-0">{{ process.type || "Process" }}</span>
                  </div>
                </div>
              </div>

              <!-- What stops being served. Not "two releases back" — the
                   commits themselves, which is what somebody recognizes. -->
              <div v-if="commits.builds.length">
                <p class="text-[0.7rem] uppercase tracking-wide text-muted mb-2">
                  {{ commits.builds.length }}
                  {{ commits.builds.length === 1 ? "commit" : "commits" }}
                  {{ commits.direction === "rollback" ? "stop" : "start" }}{{ commits.builds.length === 1 ? "s" : "" }}
                  being served
                </p>
                <div class="rounded-md border border-default overflow-hidden">
                  <div
                    v-for="commit in commits.builds"
                    :key="commit.name"
                    class="flex items-center gap-4 px-4 py-2.5 border-b border-muted last:border-0 text-sm"
                  >
                    <span class="font-mono text-xs text-muted w-20 shrink-0">{{ shortSHA(commit.git.sha) }}</span>
                    <span class="text-toned flex-1 min-w-0 truncate">{{ commitLabel(commit) }}</span>
                    <span class="text-xs text-dimmed shrink-0">{{ commit.git.author || "—" }}</span>
                  </div>
                </div>
              </div>
            </template>

            <!-- Step three. The panel stays and answers "did that work"
                 rather than returning to an unchanged list. -->
            <template v-else-if="step === 3 && target">
              <div class="flex items-baseline gap-3 flex-wrap mb-4">
                <h2 class="text-base font-medium text-highlighted">
                  <span class="font-mono">{{ target.release.name }}</span>
                  {{ landed ? "is live" : "is rolling out" }}
                </h2>
                <p class="text-xs text-muted">
                  swapped {{ timeAgo(swappedAt) }} · {{ swappedFrom }} kept in the registry
                </p>
              </div>

              <div class="rounded-md border border-default overflow-hidden mb-4">
                <div
                  v-for="check in checks"
                  :key="check.label"
                  class="flex items-center gap-3 px-4 py-2.5 border-b border-muted last:border-0 text-sm"
                >
                  <span class="size-2.5 rounded-full shrink-0" :class="checkColor(check.state)" />
                  <span class="text-toned flex-1">{{ check.label }}</span>
                  <span class="font-mono text-xs text-muted truncate max-w-[16rem]" :title="check.detail">
                    {{ check.detail }}
                  </span>
                </div>
              </div>

              <p class="text-xs text-muted max-w-prose mb-4">
                A check that has not answered yet is amber, not red: replicas that have not all rolled and a p95 that
                has not settled are things to keep watching. The release you moved off is still there — moving back is
                the same one-field change.
              </p>

              <div class="flex items-center gap-2">
                <UButton color="neutral" variant="subtle" size="sm" @click="close(false)">Back to the environment</UButton>
                <UButton
                  color="neutral"
                  variant="subtle"
                  size="sm"
                  icon="i-lucide-scroll-text"
                  :to="{ name: 'environment', params: { name: props.environment.name }, query: { section: 'workload' } }"
                  @click="close(false)"
                >
                  Watch the workload
                </UButton>
              </div>
            </template>
          </div>
        </div>

        <!-- The footnote states the mechanism in the CRD's own vocabulary,
             and the confirm is styled as the destructive action it is. -->
        <div v-if="step === 2 && target" class="border-t border-default px-5 py-3 flex items-end justify-between gap-6 flex-wrap">
          <p class="text-xs text-muted max-w-prose">
            Patches <span class="font-mono">spec.releaseRef</span> on
            <span class="font-mono">{{ props.environment.name }}</span
            >. No rebuild — the image and its variable snapshot already exist, so the swap is exact and reversible.
          </p>
          <div class="flex flex-col items-end gap-3">
            <!-- Typed confirmation is not a house style applied everywhere: it
                 is asked for exactly when "exact and reversible" stops being
                 the whole story. See gatedByName. -->
            <UFormField
              v-if="gated"
              :label="`Type ${props.environment.name} to confirm`"
              :help="target.distance >= 2 ? `${target.distance} releases back` : 'the variable snapshot changes too'"
              :ui="{ label: 'text-xs', help: 'text-xs' }"
            >
              <UInput v-model="typed" class="w-72 font-mono" size="sm" :placeholder="props.environment.name" />
            </UFormField>
            <div class="flex items-center gap-2">
              <UButton color="neutral" variant="subtle" @click="target = null">Cancel</UButton>
              <UButton color="error" icon="i-lucide-undo-2" :loading="moving" :disabled="!confirmable" @click="move">
                Make {{ target.release.name }} live
              </UButton>
            </div>
          </div>
        </div>
      </div>
    </template>
  </UModal>
</template>
