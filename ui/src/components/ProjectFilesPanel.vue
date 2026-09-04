<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type ConfigFile, type Process } from "../lib/api";
import {
  awaitingContent,
  collidingPath,
  configFileDrafts,
  configFileWrites,
  nameProblem,
  newConfigFileDraft,
  pathProblem,
  renamedFile,
  type ConfigFileDraft,
} from "../lib/files";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";

// The configuration files a project places into its workloads (#311).
//
// Software written for this platform is configured by environment variables.
// Software somebody else wrote is often configured by a file at a fixed path
// — Home Assistant's configuration.yaml, Gitea's app.ini — and until this
// existed the only way to put one there was to write it into a claimed volume
// by hand, once, out of band. It is configuration rather than storage: small,
// changing with a deploy, and frozen into every release, so a rollback
// restores the file that release ran with.
//
// **A file may hold a credential, and then its content goes one way.** The
// API answers a digest and a size for a secret file and never the content —
// so this panel holds nothing for one beyond the moment it is pasted, and the
// field is cleared as soon as the write is answered. That is why a secret
// file's editor offers "Replace content" rather than a filled-in box.
//
// It is its own section of the settings tab, with its own save, because it is
// its own concern: everything here is the project admin's, like the rest of
// that tab, and a file is not a variable.

const props = defineProps<{
  project: string;
  role?: string;
  files?: ConfigFile[];
  /** The project's workloads besides its web one, so a file can name the
   * ones that read it without anybody typing a name the platform will
   * refuse. */
  processes?: Process[];
  /** The repository file that declares some of these, where one does. A file
   * declared there is set back at every build, so a row that did not say so
   * would be a row whose Save silently does nothing. */
  declaredIn?: string;
  /** Which files that repository file names. */
  declaredNames?: string[];
}>();
const emit = defineEmits<{ saved: [] }>();

const toast = useToast();

const caller = computed(() => callerFor(props.role, props.project));
const mayEdit = computed(() => may("PATCH /api/v1/projects/{name}", caller.value));
const readOnlyReason = computed(() => refusal("PATCH /api/v1/projects/{name}", caller.value));

// Drafts, loaded once per project so the page's poll never types over
// somebody. Saving reloads them from the answer.
const drafts = ref<ConfigFileDraft[]>([]);
const loadedFor = ref("");
watch(
  () => [props.project, props.files] as const,
  () => {
    if (loadedFor.value === props.project) return;
    drafts.value = configFileDrafts(props.files);
    loadedFor.value = props.project;
  },
  { immediate: true },
);

/** Whether the repository declares this file, and so sets it back at every
 * build. */
function fromRepository(name: string): boolean {
  return Boolean(props.declaredIn) && (props.declaredNames ?? []).includes(name);
}

/** Every workload a file may name: the web process, and the project's own. */
const workloadNames = computed(() => ["web", ...(props.processes ?? []).map((p) => p.name)]);

const saving = ref(false);
const writeError = ref("");

// The editor. One file at a time, in a dialogue rather than inline, because a
// file's content is a document and a row in a table is not where one is read.
const open = ref(false);
const editing = ref<number>(-1);
const draft = ref<ConfigFileDraft>(newConfigFileDraft());
// The content typed for a secret file, held only until the write is answered.
const secretContent = ref("");

const adding = computed(() => editing.value < 0);
const problem = computed(() => nameProblem(draft.value.name) || pathProblem(draft.value.path));
const collides = computed(() => {
  const candidate = [...drafts.value];
  const at = adding.value ? candidate.push(draft.value) - 1 : ((candidate[editing.value] = draft.value), editing.value);
  return collidingPath(candidate, at);
});
const complete = computed(
  () => draft.value.name.trim() !== "" && draft.value.path.trim() !== "" && !problem.value && !collides.value,
);

function openEditor(at?: number) {
  editing.value = at ?? -1;
  draft.value = at === undefined ? newConfigFileDraft() : { ...drafts.value[at], workloads: [...drafts.value[at].workloads] };
  secretContent.value = "";
  writeError.value = "";
  open.value = true;
}

/** Close the dialogue, and forget whatever was pasted into it. */
function close() {
  open.value = false;
  secretContent.value = "";
}

/** Whether this file needs content before its workloads can start: a secret
 * one the platform holds nothing for, and nothing typed to fix that. */
const missingContent = computed(
  () => draft.value.secret && awaitingContent(draft.value) && secretContent.value === "",
);

async function save() {
  if (saving.value || !complete.value) return;
  const entry = { ...draft.value };
  const next = [...drafts.value];
  if (adding.value) next.push(entry);
  else next[editing.value] = entry;

  saving.value = true;
  writeError.value = "";
  try {
    const answered = await api.updateProject(props.project, { files: configFileWrites(next) });
    // The content second, and only once the declaration has landed: the
    // content route refuses a file that has not been declared.
    if (entry.secret && secretContent.value !== "") {
      await api.setProjectFile(props.project, entry.name.trim(), secretContent.value);
    }
    drafts.value = configFileDrafts(answered.files);
    if (entry.secret && secretContent.value !== "") {
      // The digest the answer carried predates the content write, so the
      // panel reads it again rather than showing the previous one.
      drafts.value = configFileDrafts((await api.project(props.project)).files);
    }
    close();
    toast.add({
      title: `${entry.name.trim()} saved`,
      description: entry.secret
        ? "The content is stored where the application reads it and nowhere anything reads it back. Replacing it restarts whatever reads it."
        : "It lands in the next release: what is running keeps its release's file until the next deploy.",
      color: "success",
      icon: "i-lucide-check",
    });
    emit("saved");
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
  } finally {
    saving.value = false;
  }
}

const removing = ref<number>(-1);
const removed = ref(false);

async function remove() {
  if (removing.value < 0 || removed.value) return;
  removed.value = true;
  writeError.value = "";
  const next = drafts.value.filter((_, index) => index !== removing.value);
  try {
    const answered = await api.updateProject(props.project, { files: configFileWrites(next) });
    drafts.value = configFileDrafts(answered.files);
    removing.value = -1;
    emit("saved");
  } catch (err) {
    removing.value = -1;
    writeError.value = err instanceof Error ? err.message : String(err);
  } finally {
    removed.value = false;
  }
}

/** What can be said about one file's content, which for a secret one is
 * everything the platform will answer. */
function contentSummary(file: ConfigFileDraft): string {
  if (!file.secret) return `${(file.content ?? "").length} bytes`;
  if (!file.contentHash) return "not set yet";
  return `${file.size ?? 0} bytes · ${file.contentHash}`;
}
</script>

<template>
  <div class="space-y-4 max-w-3xl">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-sm font-medium text-highlighted">Files</h2>
        <p class="text-xs text-muted mt-1">
          Configuration files <span class="font-mono">{{ project }}</span> places into its workloads. Applications
          written for this platform are configured by variables; software somebody else wrote is often configured by a
          file at a fixed path, and this is where one goes. Each is mounted read-only at its path, exactly as written —
          nothing is substituted into it.
        </p>
        <p class="text-xs text-dimmed mt-1">
          Files are frozen into every release, so a rollback restores the file that release ran with. A file marked
          secret holds a credential: its content goes in and never comes back out, and replacing it reaches what is
          already running.
        </p>
      </div>
      <UButton v-if="mayEdit" size="xs" color="neutral" variant="subtle" icon="i-lucide-plus" @click="openEditor()">
        Add a file
      </UButton>
    </div>

    <UAlert
      v-if="writeError && !open"
      color="warning"
      variant="soft"
      icon="i-lucide-info"
      :title="writeError"
      close
      @update:open="writeError = ''"
    />

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full min-w-[38rem] text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default">
            <th class="px-3 py-2 font-medium">Name</th>
            <th class="px-3 py-2 font-medium">Path</th>
            <th class="px-3 py-2 font-medium">Read by</th>
            <th class="px-3 py-2 font-medium">Content</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!drafts.length">
            <td colspan="5" class="px-3 py-8 text-center text-muted">
              No files. Software configured by one at a fixed path goes here rather than into a volume written by hand.
            </td>
          </tr>
          <tr v-for="(file, index) in drafts" :key="file.name" class="border-b border-muted last:border-0">
            <td class="px-3 py-2">
              <p class="text-highlighted font-mono">{{ file.name }}</p>
              <UBadge v-if="file.secret" color="neutral" variant="subtle" size="sm" class="mt-1">secret</UBadge>
              <!-- The repository wins for a file it declares, so a change made
                   here is set back by the next build. It is said on the row
                   rather than only at the save, because that is where somebody
                   is about to make it. -->
              <p v-if="fromRepository(file.name)" class="text-xs text-dimmed mt-1">
                declared in <span class="font-mono">{{ declaredIn }}</span>
              </p>
            </td>
            <td class="px-3 py-2 font-mono text-muted">{{ file.path }}</td>
            <td class="px-3 py-2">
              <template v-if="file.workloads.length">
                <UBadge
                  v-for="workload in file.workloads"
                  :key="workload"
                  color="neutral"
                  variant="subtle"
                  size="sm"
                  class="font-mono mr-1"
                >
                  {{ workload }}
                </UBadge>
              </template>
              <span v-else class="text-xs text-dimmed">everything this project runs</span>
            </td>
            <td class="px-3 py-2">
              <span v-if="awaitingContent(file)" class="text-xs text-warning">
                not set yet — what reads it will not start
              </span>
              <span v-else class="text-xs text-dimmed font-mono">{{ contentSummary(file) }}</span>
            </td>
            <td class="px-3 py-2 text-right whitespace-nowrap">
              <UButton
                v-if="mayEdit"
                color="neutral"
                variant="link"
                size="xs"
                class="px-0 mr-3"
                @click="openEditor(index)"
              >
                Edit
              </UButton>
              <UButton
                v-if="mayEdit"
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-trash-2"
                :aria-label="`Remove ${file.name}`"
                @click="removing = index"
              />
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <p v-if="!mayEdit && readOnlyReason" class="text-xs text-muted">{{ readOnlyReason }}.</p>

    <UModal
      v-model:open="open"
      :title="adding ? 'Add a file' : `Edit ${draft.name}`"
      description="What is in the file, where it appears inside the application, and which of this project's workloads read it."
      @update:open="(shown: boolean) => { if (!shown) close(); }"
    >
      <template #body>
        <form class="space-y-4" @submit.prevent="save">
          <UAlert v-if="writeError" color="warning" variant="soft" icon="i-lucide-info" :title="writeError" />
          <UFormField
            label="Name"
            help="A name for the file, not its path. Letters, digits, and - _ . — it is how everything else refers to it."
            :error="nameProblem(draft.name) || undefined"
          >
            <UInput v-model="draft.name" placeholder="configuration" class="w-full font-mono" autocomplete="off" />
          </UFormField>
          <UFormField
            label="Path"
            help="Where the file appears inside the application: absolute, naming the file itself. Everything else in that directory stays as the image left it. Leave it empty for a file that is only seeded into a volume — a mounted file is read-only, and one mounted where the seed writes would shadow the copy the application then owns."
            :error="pathProblem(draft.path) || (collides ? 'Another file is already mounted there.' : undefined)"
          >
            <UInput
              v-model="draft.path"
              placeholder="/config/configuration.yaml"
              class="w-full font-mono"
              autocomplete="off"
            />
          </UFormField>
          <UFormField
            label="Read by"
            help="The workloads that get the file. Leave it empty and everything this project runs gets it."
          >
            <USelectMenu
              v-model="draft.workloads"
              :items="workloadNames"
              multiple
              placeholder="Everything this project runs"
              class="w-full"
            />
          </UFormField>
          <UFormField
            help="A file that holds a credential. Its content is stored where the application reads it and nowhere anything reads it back — so it is never shown here again, and replacing it means pasting the new one."
          >
            <UCheckbox v-model="draft.secret" label="This file holds a credential" :disabled="!adding" />
          </UFormField>
          <UFormField
            v-if="!draft.secret"
            label="Content"
            help="The file, exactly as it will appear. Nothing is substituted into it — a value that changes per environment belongs in a variable the application reads."
          >
            <UTextarea v-model="draft.content" :rows="10" class="w-full font-mono" autocomplete="off" />
          </UFormField>
          <UFormField
            v-else
            label="Content"
            :help="
              awaitingContent(draft)
                ? 'Nothing is stored yet, so what reads this file will not start until it is.'
                : 'The content that replaces the one stored now. There is nothing to prefill it with.'
            "
          >
            <UTextarea
              v-model="secretContent"
              :rows="10"
              placeholder="paste the file"
              class="w-full font-mono"
              autocomplete="off"
            />
          </UFormField>
          <p v-if="renamedFile(draft) && draft.secret" class="text-xs text-warning">
            Content is stored under the file's name, so renaming it leaves the stored content behind. Paste it again.
          </p>
          <p v-if="missingContent" class="text-xs text-warning">
            Saving without content declares the file and leaves it empty on the platform — what reads it will not start.
          </p>
        </form>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="close">Cancel</UButton>
          <UButton :disabled="!complete" :loading="saving" icon="i-lucide-check" @click="save">
            {{ adding ? "Add file" : "Save file" }}
          </UButton>
        </div>
      </template>
    </UModal>

    <UModal
      :open="removing >= 0"
      :title="`Remove ${removing >= 0 ? drafts[removing]?.name : ''}?`"
      description="The declaration goes and the content goes with it. What is already running keeps the file its release was built with; the removal lands in the next release."
      @update:open="(shown: boolean) => { if (!shown) removing = -1; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="removing = -1">Cancel</UButton>
          <UButton color="error" :loading="removed" icon="i-lucide-trash-2" @click="remove">Remove file</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
