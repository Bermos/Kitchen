<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type Process } from "../lib/api";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";
import {
  type WorkloadDraft,
  WORKLOAD_TYPES,
  addressed,
  newWorkloadDraft,
  originLabel,
  processWrites,
  runsOnce,
  workloadDrafts,
  workloadProblems,
} from "../lib/workloads";

// What this project runs besides its web process, and how it is changed.
//
// **For a project with no repository this is the primary surface, not a
// fallback.** A repository declares its workloads in `kitchen.json`, committed
// beside the code and read at every build; a project whose source is an image
// has no repository, so it has no file, and the API, this panel and
// `kitchen processes set` are the only route its unit is described by (#310).
//
// For a project that *does* have a repository the file still wins for whatever
// it names, so when it declares `processes` this reads rather than edits, in
// the same words the notice above the settings form uses: changing it here
// would hold only until the next build sets it back.
//
// It is its own component and its own section for the ordinary reason a panel
// is: the settings form is one form with one Save, and this is a list of
// records with an add and a remove of its own. It writes the same route the
// form does — the workload list is a project setting, and what a project runs
// and how much of it is one decision made by one person.

const props = defineProps<{
  project: string;
  role?: string;
  processes?: Process[];
  /** Whether this project is built from a repository. A project with none has
   * no image of its own for a workload to run, so a new workload there starts
   * as a vendored one and the build fields are not offered at all. */
  builtHere: boolean;
  /** The repository's `kitchen.json`, where one declared this list — its path,
   * for the notice. Absent means the project's own declaration stands. */
  declaredIn?: string;
}>();
const emit = defineEmits<{ saved: [] }>();

const toast = useToast();

const caller = computed(() => callerFor(props.role, props.project));
const mayWrite = computed(() => may("PATCH /api/v1/projects/{name}", caller.value));
const readOnlyReason = computed(() => refusal("PATCH /api/v1/projects/{name}", caller.value));
// The repository's declaration is not a permission — an admin may still write
// it — but editing what the next build overwrites is a change that undoes
// itself, so the form reads instead and says where to make it.
const fileOwnsIt = computed(() => Boolean(props.declaredIn));
const mayEdit = computed(() => mayWrite.value && !fileOwnsIt.value);

// Drafts load once per project, not on every answer: the project view polls
// every ten seconds and a reload on each would type over somebody mid-edit.
const loadedFor = ref("");
const drafts = ref<WorkloadDraft[]>([]);
watch(
  () => [props.project, props.processes] as const,
  ([name, processes]) => {
    if (name && name !== loadedFor.value) {
      loadedFor.value = name;
      drafts.value = workloadDrafts(processes);
    }
  },
  { immediate: true },
);

const open = ref<string[]>([]);
function toggle(key: string) {
  open.value = open.value.includes(key) ? open.value.filter((k) => k !== key) : [...open.value, key];
}

function addWorkload() {
  const draft = newWorkloadDraft(props.builtHere ? "project" : "image");
  drafts.value.push(draft);
  open.value = [...open.value, draft.key];
}
function removeWorkload(index: number) {
  drafts.value.splice(index, 1);
}

const typeOptions = WORKLOAD_TYPES.map((value) => ({
  label: {
    worker: "worker — runs continuously, nothing addresses it",
    service: "service — runs continuously, its siblings address it",
    cron: "scheduled job — runs on a cron expression, in UTC",
    task: "task — runs once per deploy, before the release takes traffic",
  }[value],
  value: value as string,
}));

const originOptions = computed(() =>
  [
    {
      label: "this project's own image, run with another command",
      value: "project",
      shown: props.builtHere,
    },
    { label: "built from a directory of this repository", value: "build", shown: props.builtHere },
    { label: "an image somebody else built", value: "image", shown: true },
  ]
    .filter((option) => option.shown)
    .map(({ label, value }) => ({ label, value })),
);

const previewsOptions = [
  { label: "the default for its type", value: "default" },
  { label: "runs in previews", value: "yes" },
  { label: "stays out of previews", value: "no" },
];

const strategyOptions = [
  { label: "auto — detect the framework", value: "auto" },
  { label: "dockerfile", value: "dockerfile" },
  { label: "buildpacks", value: "buildpacks" },
];

const concurrencyOptions = [
  { label: "Forbid — a run that is due waits for the last one", value: "Forbid" },
  { label: "Allow — two runs may overlap", value: "Allow" },
  { label: "Replace — the running one is stopped", value: "Replace" },
];

const problems = computed(() => workloadProblems(drafts.value));

// What a row says before it is opened, so a list of six is readable without
// expanding any of them.
function summary(draft: WorkloadDraft): string {
  const parts = [draft.type];
  if (draft.type === "cron" && draft.schedule.trim()) parts.push(draft.schedule.trim());
  else if (draft.type === "task") parts.push("once per deploy");
  else if (draft.replicas.trim() === "0") parts.push("parked");
  else if (draft.replicas.trim()) parts.push(`${draft.replicas.trim()}×`);
  if (addressed(draft.type) && draft.port.trim()) parts.push(`port ${draft.port.trim()}`);
  parts.push(originLabel(draft));
  return parts.join(" · ");
}

const saving = ref(false);
async function save() {
  if (!mayEdit.value || saving.value || problems.value.length) return;
  saving.value = true;
  try {
    const saved = await api.updateProject(props.project, { processes: processWrites(drafts.value) });
    drafts.value = workloadDrafts(saved.processes);
    open.value = [];
    toast.add({
      title: "Workloads saved",
      description:
        "They reach an environment through the next release: what is running keeps the workloads its own release declared.",
      color: "success",
      icon: "i-lucide-check",
    });
    emit("saved");
  } catch (err) {
    toast.add({
      title: "Saving the workloads failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <div class="space-y-4 max-w-3xl">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-sm font-medium text-highlighted">Workloads</h2>
        <p class="text-xs text-muted mt-1">
          What <span class="font-mono">{{ project }}</span> runs besides the web process: the queue workers, the
          scheduled jobs, the services the rest of it talks to, and the work that runs once before a release takes
          traffic. The web process is not here — it is the project's own runtime above, and it is the one workload with
          a URL.
        </p>
        <p v-if="!builtHere" class="text-xs text-dimmed mt-1">
          Nothing here is built by this platform, so every workload names an image somebody else published — and
          deploys, previews and rolls back with the project's own as one unit.
        </p>
      </div>
      <UButton
        v-if="mayEdit"
        color="neutral"
        variant="subtle"
        size="xs"
        icon="i-lucide-plus"
        @click="addWorkload"
      >
        Add workload
      </UButton>
    </div>

    <!-- The repository has taken this over: the same sentence the notice above
         the settings form uses, because it is the same rule one list down. -->
    <div v-if="fileOwnsIt" class="rounded-md border border-info/40 bg-info/5 px-5 py-4">
      <div class="flex items-start gap-2">
        <UIcon name="i-lucide-file-code" class="size-4 text-info mt-0.5 shrink-0" />
        <div class="min-w-0 space-y-1">
          <p class="text-sm font-medium text-highlighted">
            <span class="font-mono">{{ declaredIn }}</span> decides these
          </p>
          <p class="text-xs text-toned">
            The repository declares this project's workloads, read at every commit, and its list replaces the
            project's wholesale — a workload the file does not name is a workload the commit does not have. Changing
            one here would hold until the next build set it back, so this reads instead. Change the file.
          </p>
        </div>
      </div>
    </div>

    <form class="space-y-4" @submit.prevent="save">
      <div class="rounded-md border border-default bg-muted p-5 space-y-3">
        <p v-if="!drafts.length" class="text-xs text-muted">
          None yet. This project is its web process and nothing else.
        </p>

        <div v-for="(draft, index) in drafts" :key="draft.key" class="rounded-md border border-default bg-default">
          <div class="flex items-center gap-2 px-4 py-2">
            <UButton
              color="neutral"
              variant="ghost"
              size="xs"
              :icon="open.includes(draft.key) ? 'i-lucide-chevron-down' : 'i-lucide-chevron-right'"
              :aria-label="open.includes(draft.key) ? 'Collapse' : 'Expand'"
              @click="toggle(draft.key)"
            />
            <div class="min-w-0 flex-1">
              <p class="text-sm font-mono text-highlighted truncate">{{ draft.name || "unnamed" }}</p>
              <p class="text-xs text-muted truncate">{{ summary(draft) }}</p>
            </div>
            <UButton
              v-if="mayEdit"
              color="neutral"
              variant="ghost"
              size="xs"
              icon="i-lucide-x"
              aria-label="Remove workload"
              @click="removeWorkload(index)"
            />
          </div>

          <div v-if="open.includes(draft.key)" class="border-t border-default px-4 py-4 space-y-4">
            <div class="grid gap-4 sm:grid-cols-2">
              <UFormField label="Name" help="Lower-case letters, digits and dashes. It names everything this workload runs as.">
                <UInput v-model="draft.name" :disabled="!mayEdit" class="w-full font-mono" placeholder="worker" />
              </UFormField>
              <UFormField label="Type">
                <USelect v-model="draft.type" :items="typeOptions" :disabled="!mayEdit" class="w-full" />
              </UFormField>
            </div>

            <div class="grid gap-4 sm:grid-cols-2">
              <UFormField
                label="Command"
                help="One word per line. It replaces the image's entrypoint; leave it empty to run whatever the image starts."
              >
                <UTextarea v-model="draft.command" :disabled="!mayEdit" :rows="2" class="w-full font-mono" />
              </UFormField>
              <UFormField label="Arguments" help="One word per line. An argument with a space in it is one line.">
                <UTextarea v-model="draft.args" :disabled="!mayEdit" :rows="2" class="w-full font-mono" />
              </UFormField>
            </div>

            <UFormField label="Image" :help="draft.origin === 'image' ? 'Pinned to a version: a tag, a digest, or both.' : undefined">
              <USelect v-model="draft.origin" :items="originOptions" :disabled="!mayEdit" class="w-full" />
            </UFormField>

            <div v-if="draft.origin === 'image'" class="grid gap-4 sm:grid-cols-2">
              <UFormField label="Repository" help="Registry host included, and without a tag or a digest.">
                <UInput
                  v-model="draft.imageRepository"
                  :disabled="!mayEdit"
                  class="w-full font-mono"
                  placeholder="docker.io/library/redis"
                />
              </UFormField>
              <UFormField label="Tag">
                <UInput v-model="draft.imageTag" :disabled="!mayEdit" class="w-full font-mono" placeholder="7.4" />
              </UFormField>
              <UFormField label="Digest" help="Pins the exact content. With a tag it means: this tag, and it must still be this.">
                <UInput v-model="draft.imageDigest" :disabled="!mayEdit" class="w-full font-mono" placeholder="sha256:…" />
              </UFormField>
              <UFormField label="Pulled with" help="A connection holding a login for that registry. Leave it empty for a public image.">
                <UInput v-model="draft.imageConnection" :disabled="!mayEdit" class="w-full font-mono" />
              </UFormField>
            </div>

            <div v-else-if="draft.origin === 'build'" class="grid gap-4 sm:grid-cols-2">
              <UFormField label="Directory" help="The directory of the repository this workload is built from.">
                <UInput
                  v-model="draft.buildRootDirectory"
                  :disabled="!mayEdit"
                  class="w-full font-mono"
                  placeholder="services/api"
                />
              </UFormField>
              <UFormField label="Strategy">
                <USelect v-model="draft.buildStrategy" :items="strategyOptions" :disabled="!mayEdit" class="w-full" />
              </UFormField>
              <UFormField label="Dockerfile" help="Relative to this workload's own directory.">
                <UInput
                  v-model="draft.buildDockerfilePath"
                  :disabled="!mayEdit"
                  class="w-full font-mono"
                  placeholder="Dockerfile"
                />
              </UFormField>
              <UFormField label="Stage" help="The stage of a multi-stage Dockerfile to ship. Empty is the project's own.">
                <UInput v-model="draft.buildDockerfileTarget" :disabled="!mayEdit" class="w-full font-mono" />
              </UFormField>
            </div>

            <div class="grid gap-4 sm:grid-cols-3">
              <UFormField
                v-if="addressed(draft.type)"
                label="Port"
                help="What it listens on, and what its siblings reach it on — the same number."
              >
                <UInput v-model="draft.port" :disabled="!mayEdit" type="number" class="w-full" />
              </UFormField>
              <UFormField
                v-if="!runsOnce(draft.type)"
                label="Replicas"
                help="0 is declared and parked: turned off without losing its command."
              >
                <UInput v-model="draft.replicas" :disabled="!mayEdit || draft.singleton" type="number" class="w-full" />
              </UFormField>
              <UFormField v-if="draft.type === 'cron'" label="Schedule" help="Five fields, read in UTC.">
                <UInput v-model="draft.schedule" :disabled="!mayEdit" class="w-full font-mono" placeholder="0 3 * * *" />
              </UFormField>
              <UFormField
                v-if="runsOnce(draft.type)"
                label="Timeout"
                help="How long one run may take. An hour by default; a deploy waits this long for a task."
              >
                <UInput v-model="draft.timeout" :disabled="!mayEdit" class="w-full font-mono" placeholder="30m" />
              </UFormField>
              <UFormField label="CPU" help="What one replica, or one run, asks for — 250m.">
                <UInput v-model="draft.cpu" :disabled="!mayEdit" class="w-full font-mono" />
              </UFormField>
              <UFormField label="Memory" help="512Mi.">
                <UInput v-model="draft.memory" :disabled="!mayEdit" class="w-full font-mono" />
              </UFormField>
            </div>

            <UFormField v-if="draft.type === 'cron'" label="When a run is due and the last one has not finished">
              <USelect
                v-model="draft.concurrencyPolicy"
                :items="concurrencyOptions"
                :disabled="!mayEdit"
                class="w-full"
              />
            </UFormField>

            <UFormField label="Preview environments">
              <USelect v-model="draft.previews" :items="previewsOptions" :disabled="!mayEdit" class="w-full" />
            </UFormField>

            <USwitch
              v-if="!runsOnce(draft.type)"
              v-model="draft.singleton"
              :disabled="!mayEdit"
              label="Never two at once"
              description="Deploys stop the old copy before starting the new one instead of overlapping them, which is what a poller or an ingest loop needs. It runs one replica."
            />

            <USwitch
              v-if="!runsOnce(draft.type)"
              v-model="draft.health"
              :disabled="!mayEdit"
              label="Check that it is working"
              description="A workload with no check is alive for as long as its process is running. One that serves a health listener says which port it is on — it publishes none of its own."
            />
            <div v-if="draft.health && !runsOnce(draft.type)" class="grid gap-4 sm:grid-cols-3">
              <UFormField label="Path" help="Empty is a plain connect rather than a request.">
                <UInput v-model="draft.healthPath" :disabled="!mayEdit" class="w-full font-mono" placeholder="/healthz" />
              </UFormField>
              <UFormField label="Port">
                <UInput v-model="draft.healthPort" :disabled="!mayEdit" type="number" class="w-full" />
              </UFormField>
              <UFormField label="Every (seconds)" help="Empty takes the platform's own.">
                <UInput v-model="draft.healthPeriod" :disabled="!mayEdit" type="number" class="w-full" />
              </UFormField>
              <UFormField label="Timeout (seconds)">
                <UInput v-model="draft.healthTimeout" :disabled="!mayEdit" type="number" class="w-full" />
              </UFormField>
              <UFormField label="Failures before restart">
                <UInput v-model="draft.healthFailures" :disabled="!mayEdit" type="number" class="w-full" />
              </UFormField>
              <UFormField label="Failures allowed while starting">
                <UInput v-model="draft.healthStartupFailures" :disabled="!mayEdit" type="number" class="w-full" />
              </UFormField>
            </div>
          </div>
        </div>
      </div>

      <div v-if="problems.length" class="rounded-md border border-warning/40 bg-warning/5 px-5 py-4 space-y-1">
        <p v-for="problem in problems" :key="problem" class="text-xs text-toned">{{ problem }}</p>
      </div>

      <div v-if="mayEdit" class="flex items-center justify-between gap-4">
        <p class="text-xs text-dimmed">
          The list replaces the one stored, so removing a workload here removes it. It reaches an environment through
          the next release.
        </p>
        <UButton type="submit" :loading="saving" :disabled="problems.length > 0" icon="i-lucide-check">
          Save workloads
        </UButton>
      </div>
      <p v-else-if="!fileOwnsIt && readOnlyReason" class="text-xs text-muted">{{ readOnlyReason }}.</p>
    </form>
  </div>
</template>
