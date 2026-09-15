<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type EnvVar } from "../lib/api";
import {
  type EnvVarDraft,
  envVarDrafts,
  envVarWrites,
  newEnvVarDraft,
  renamed,
  withoutShownValues,
  withShownValues,
} from "../lib/envvars";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";

// A project's environment variables, on a route and a role of their own.
//
// **This is the developer's day job, and the project's settings are the
// admin's.** A whole route is the unit of authorization, so the two are two
// routes: `PATCH /projects/{name}/env` wants `developer`, `PATCH
// /projects/{name}` wants `admin`, and the latter now refuses a body carrying
// `env` outright rather than dropping the field. That is why this is a panel
// and not a section of the settings form — a developer who is not an admin has
// no settings form, and had no way to change a variable while it lived inside
// one.
//
// **Reading the list is every role's, and the tab is keyed to that.** `GET
// /projects/{name}` is a viewer's route and already carries it — names,
// whether each has a value, and the references the backed ones were written
// as — so withholding the screen would be the dashboard enforcing something
// the API does not. A viewer gets exactly this, with the add, replace, remove
// and save affordances gone rather than disabled.
//
// **Reading a *value* is the developer's, and it is asked for** (#598). The
// literals ride a route of their own at the role that may replace them, and
// the platform records the read — so this screen does not fetch them with the
// page. The project view polls every ten seconds; a panel that carried the
// values would put a line in the audit log every ten seconds and leave a
// credential-shaped string on a screen nobody is looking at. "Show values" is
// one read, and "Hide" takes them back off. A variable reading a secret or a
// claim is untouched either way: it draws the reference it names, because
// that is what it holds and the platform resolves nothing.
//
// The list replaces the stored one wholesale, so every variable has to be in
// what is sent. Values are the exception, and deliberately: a variable whose
// `value` the request leaves out keeps the one it already has. That is the
// bargain that lets the whole list be replaced without the dashboard ever
// holding a value it was not given, and revealing one does not change it — a
// value drawn on the screen is not a draft, so replacing one still means
// typing the new one.

const props = defineProps<{ project: string; role?: string; env?: EnvVar[] }>();
const emit = defineEmits<{ saved: [] }>();

const toast = useToast();

const caller = computed(() => callerFor(props.role, props.project));
const maySave = computed(() => may("PATCH /api/v1/projects/{name}/env", caller.value));
const mayReveal = computed(() => may("GET /api/v1/projects/{name}/env", caller.value));
// Why not, in the words the API refuses in — under the list, where the save
// button would have been.
const readOnlyReason = computed(() => refusal("PATCH /api/v1/projects/{name}/env", caller.value));

// Whether the values were asked for, rather than whether any arrived: a
// project whose every variable reads a secret is answered no literals at all,
// and the control still has to become "Hide".
const shown = ref(false);

// Drafts are loaded once per project, not on every payload: the project view
// polls every ten seconds, and a re-load on each answer would type over
// somebody mid-edit.
const loadedFor = ref("");
const drafts = ref<EnvVarDraft[]>([]);
watch(
  () => [props.project, props.env] as const,
  ([name, env]) => {
    if (name && name !== loadedFor.value) {
      loadedFor.value = name;
      drafts.value = envVarDrafts(env);
      shown.value = false;
    }
  },
  { immediate: true },
);

// Showing the values is one read, made when it is asked for. Hiding them takes
// them off the screen and asks again next time — there is nothing kept, so a
// page left open does not go on holding them.
const revealing = ref(false);
async function showValues() {
  if (!mayReveal.value || revealing.value) return;
  revealing.value = true;
  try {
    drafts.value = withShownValues(drafts.value, await api.projectEnvValues(props.project));
    shown.value = true;
  } catch (err) {
    toast.add({
      title: "Reading the values failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    revealing.value = false;
  }
}
function hideValues() {
  drafts.value = withoutShownValues(drafts.value);
  shown.value = false;
}

function addEnvVar() {
  drafts.value.push(newEnvVarDraft());
}
function removeEnvVar(index: number) {
  drafts.value.splice(index, 1);
}
// Replacing a value is a deliberate act: the field opens empty, because there
// is nothing to prefill it with. "Keep" closes it again and the stored value
// stays — the same undo the connection modal's blank credential field is.
function replaceValue(envVar: EnvVarDraft) {
  envVar.value = "";
}
function keepValue(envVar: EnvVarDraft) {
  envVar.value = undefined;
}
function replacePreviewValue(envVar: EnvVarDraft) {
  envVar.previewValue = "";
}
function keepPreviewValue(envVar: EnvVarDraft) {
  envVar.previewValue = undefined;
}

const saving = ref(false);
async function save() {
  // The save control does not exist without the role, and neither does the
  // form's only submit path — but a role that changes under an open page is
  // exactly the case the rest of the dashboard guards, so the write checks
  // too rather than trusting the render.
  if (!maySave.value || saving.value) return;
  saving.value = true;
  try {
    const saved = await api.updateProjectEnv(props.project, envVarWrites(drafts.value));
    // The answer is the project, so the new list goes back under the form
    // without a second read — and the typed values are gone from the drafts
    // with it. So is anything that had been revealed: it is now what the
    // platform held a moment ago, and saying so would take a second read.
    drafts.value = envVarDrafts(saved.env);
    shown.value = false;
    toast.add({
      title: "Environment variables saved",
      description: "They land in the next release: what is running keeps its release's snapshot until the next deploy.",
      color: "success",
      icon: "i-lucide-check",
    });
    emit("saved");
  } catch (err) {
    toast.add({
      title: "Saving the environment variables failed",
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
        <h2 class="text-sm font-medium text-highlighted">Environment variables</h2>
        <p class="text-xs text-muted mt-1">
          What <span class="font-mono">{{ project }}</span> runs with. A variable that has a value shows
          <span class="font-mono">•••• set</span><template v-if="mayReveal">, and <strong>Show values</strong> reads
            what it is — a separate request, which the platform records</template><template
            v-else-if="maySave"
          >, and replacing it means typing the new one</template>. A variable reading a secret or a claim shows the
          reference it names and never what is behind it. They land in new releases: what is running keeps its
          release's snapshot until the next deploy.
        </p>
        <p class="text-xs text-dimmed mt-1">
          <span class="font-mono">PORT</span> is the platform's, is set on every environment, and is injected ahead of
          these — so a variable named PORT here still wins.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <UButton
          v-if="mayReveal && drafts.length"
          color="neutral"
          variant="ghost"
          size="xs"
          :loading="revealing"
          :icon="shown ? 'i-lucide-eye-off' : 'i-lucide-eye'"
          @click="shown ? hideValues() : showValues()"
        >
          {{ shown ? "Hide" : "Show values" }}
        </UButton>
        <UButton
          v-if="maySave"
          color="neutral"
          variant="subtle"
          size="xs"
          icon="i-lucide-plus"
          @click="addEnvVar"
        >
          Add variable
        </UButton>
      </div>
    </div>

    <form class="space-y-4" @submit.prevent="save">
      <div class="rounded-md border border-default bg-muted p-5 space-y-4">
        <p v-if="!drafts.length" class="text-xs text-muted">None yet.</p>
        <div v-for="(envVar, index) in drafts" :key="index" class="flex items-start gap-2 flex-wrap sm:flex-nowrap">
          <UInput v-if="maySave" v-model="envVar.name" placeholder="NAME" class="w-full sm:w-44 font-mono" />
          <p v-else class="w-full sm:w-44 font-mono text-sm text-highlighted min-h-8 pt-1.5 truncate" :title="envVar.name">
            {{ envVar.name }}
          </p>
          <div v-if="!envVar.fromSecret && !envVar.fromClaim" class="flex-1 min-w-40 grid gap-2 sm:grid-cols-2">
            <!-- The value: shown as presence, replaced by typing. -->
            <div class="flex items-center gap-2 min-h-8">
              <UInput
                v-if="maySave && envVar.value !== undefined"
                v-model="envVar.value"
                :placeholder="envVar.set ? 'new value' : 'value'"
                autocomplete="off"
                class="flex-1 min-w-0 font-mono"
              />
              <!-- A revealed value is drawn as itself, once it has been asked for
                   and where the variable has not been renamed away from it. -->
              <p
                v-else-if="envVar.shown && envVar.set && !renamed(envVar)"
                class="flex-1 min-w-0 font-mono text-sm text-highlighted break-all"
              >
                {{ envVar.shown.value }}
              </p>
              <UBadge
                v-else
                :color="envVar.set && renamed(envVar) ? 'warning' : 'neutral'"
                variant="subtle"
                size="sm"
                class="font-mono"
              >
                {{ envVar.set ? (renamed(envVar) ? "renamed — set again" : "•••• set") : "no value" }}
              </UBadge>
              <UButton
                v-if="maySave && envVar.value === undefined"
                color="neutral"
                variant="link"
                size="xs"
                class="px-0"
                @click="replaceValue(envVar)"
              >
                {{ envVar.set && !renamed(envVar) ? "Replace" : "Set" }}
              </UButton>
              <UButton
                v-else-if="maySave && envVar.set"
                color="neutral"
                variant="link"
                size="xs"
                class="px-0"
                @click="keepValue(envVar)"
              >
                Keep
              </UButton>
            </div>
            <!-- The preview value, on the same terms. -->
            <div class="flex items-center gap-2 min-h-8">
              <UInput
                v-if="maySave && envVar.previewValue !== undefined"
                v-model="envVar.previewValue"
                :placeholder="envVar.previewSet ? 'new preview value' : 'preview value (optional)'"
                autocomplete="off"
                class="flex-1 min-w-0 font-mono"
              />
              <p
                v-else-if="envVar.shown && envVar.previewSet && !renamed(envVar)"
                class="flex-1 min-w-0 font-mono text-sm text-highlighted break-all"
              >
                {{ envVar.shown.previewValue }}
              </p>
              <UBadge
                v-else
                :color="envVar.previewSet && renamed(envVar) ? 'warning' : 'neutral'"
                variant="subtle"
                size="sm"
                class="font-mono"
              >
                {{
                  envVar.previewSet ? (renamed(envVar) ? "renamed — set again" : "•••• preview set") : "no preview value"
                }}
              </UBadge>
              <UButton
                v-if="maySave && envVar.previewValue === undefined"
                color="neutral"
                variant="link"
                size="xs"
                class="px-0"
                @click="replacePreviewValue(envVar)"
              >
                {{ envVar.previewSet && !renamed(envVar) ? "Replace" : "Set" }}
              </UButton>
              <UButton
                v-else-if="maySave && envVar.previewSet"
                color="neutral"
                variant="link"
                size="xs"
                class="px-0"
                @click="keepPreviewValue(envVar)"
              >
                Keep
              </UButton>
            </div>
          </div>
          <UBadge v-else color="neutral" variant="subtle" size="sm" class="font-mono mt-1.5 flex-1">
            {{ envVar.fromSecret ? `secret ${envVar.fromSecret.name}/${envVar.fromSecret.key}` : `claim ${envVar.fromClaim!.name}/${envVar.fromClaim!.key}` }}
          </UBadge>
          <UButton
            v-if="maySave"
            color="neutral"
            variant="ghost"
            size="xs"
            icon="i-lucide-x"
            aria-label="Remove variable"
            class="mt-1"
            @click="removeEnvVar(index)"
          />
        </div>
      </div>

      <div v-if="maySave" class="flex justify-end">
        <UButton type="submit" :loading="saving" icon="i-lucide-check">Save variables</UButton>
      </div>
      <p v-else-if="readOnlyReason" class="text-xs text-muted">{{ readOnlyReason }}.</p>
    </form>
  </div>
</template>
