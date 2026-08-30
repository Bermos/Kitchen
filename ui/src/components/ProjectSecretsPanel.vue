<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type EnvVar, type ProjectSecret } from "../lib/api";
import { envVarDrafts, envVarWrites } from "../lib/envvars";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";
import { useAsync } from "../lib/useAsync";

// A project's own secrets: the credentials Kitchen did not mint — a database
// the project runs itself, a third-party API key, an SMTP password.
//
// It sits under the environment variables because that is the choice it
// replaces. Until this existed, a credential had two homes: a variable's
// value, where the whole project can read it and the change log records it,
// or a Secret somebody wrote by hand with cluster access. Now it has one, and
// the variable holds a reference instead.
//
// **The value goes one way and there is nothing here that could show one.**
// The API answers a name and the reference; this panel never holds a value
// beyond the moment it is typed, and the field is cleared as soon as the write
// is answered. That is why setting and rotating are one control with one
// label: a rotation is the same act, and a panel that had to say which one it
// was doing would have to know whether a value is there — which it does.
//
// Reading the list is a viewer's, like reading the variables it belongs to;
// setting and removing are a developer's, like changing them. What a viewer
// gets is this screen with the write affordances gone rather than disabled.

const props = defineProps<{ project: string; role?: string; env?: EnvVar[] }>();
const emit = defineEmits<{ saved: [] }>();

const toast = useToast();

const caller = computed(() => callerFor(props.role, props.project));
const maySet = computed(() => may("PUT /api/v1/projects/{name}/secrets/{secret}", caller.value));
const mayRemove = computed(() => may("DELETE /api/v1/projects/{name}/secrets/{secret}", caller.value));
const readOnlyReason = computed(() => refusal("PUT /api/v1/projects/{name}/secrets/{secret}", caller.value));
// Pointing a variable at a secret is a write to the variables, not to the
// secrets, so it asks the variables' own route — the same one the panel above
// this uses. Both are a developer's; naming the route here rather than reusing
// the answer above keeps the control honest if that ever stops being true.
const mayUse = computed(() => may("PATCH /api/v1/projects/{name}/env", caller.value));

const { data, error, loading, refresh } = useAsync(() => api.projectSecrets(props.project));
watch(
  () => props.project,
  () => void refresh(),
);
const secrets = computed(() => data.value ?? []);

// One line, in the API's own words, for whatever the last write ran into: a
// name the API spells its own rules for (400), or a secret an environment
// variable still reads (409), whose message is the list of variables to point
// somewhere else first.
const writeError = ref("");

// Setting, and rotating, which are the same dialogue. `rotating` carries the
// secret whose value is being replaced, and null means a new one.
const open = ref(false);
const rotating = ref<ProjectSecret | null>(null);
const name = ref("");
const value = ref("");
const saving = ref(false);

// The API's own key rule, checked here so a space is a line under the field
// rather than a round trip. The API still decides.
const namePattern = /^[-._a-zA-Z0-9]+$/;
const nameProblem = computed(() => {
  const typed = name.value.trim();
  if (!typed || namePattern.test(typed)) return "";
  return "Letters, digits, and - _ . only — it is the key a variable references.";
});

function openSet(secret?: ProjectSecret) {
  rotating.value = secret ?? null;
  name.value = secret?.name ?? "";
  value.value = "";
  writeError.value = "";
  open.value = true;
}

/** Close the dialogue, and forget whatever was typed into it. */
function close() {
  open.value = false;
  value.value = "";
}

async function save() {
  const typed = name.value.trim();
  if (saving.value || !typed || nameProblem.value || !value.value) return;
  saving.value = true;
  writeError.value = "";
  try {
    await api.setProjectSecret(props.project, typed, value.value);
    close();
    await refresh();
    toast.add({
      title: rotating.value ? `${typed} replaced` : `${typed} set`,
      description: "It reaches what is already running — the platform restarts whatever reads it.",
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

/** The variables that already read a secret, which is what makes the row
 * answer "is this one actually in use". */
function readersOf(secret: ProjectSecret): string[] {
  return (props.env ?? [])
    .filter((v) => v.fromSecret?.name === secret.reference.name && v.fromSecret?.key === secret.reference.key)
    .map((v) => v.name);
}

// Pointing a variable at a secret. It is here rather than in the variables
// panel because this is where somebody has just made one and is looking for
// what to do with it — and because the variable's whole content is the
// reference, which this side already has.
const using = ref<ProjectSecret | null>(null);
const variableName = ref("");
const used = ref(false);

function openUse(secret: ProjectSecret) {
  using.value = secret;
  variableName.value = secret.name;
  writeError.value = "";
}

async function use() {
  const secret = using.value;
  const named = variableName.value.trim();
  if (!secret || !named || used.value) return;
  used.value = true;
  writeError.value = "";
  try {
    // The write replaces the whole list, so every variable goes back — by
    // name alone, which is what keeps the values this dashboard has never
    // been shown.
    const kept = envVarWrites(envVarDrafts(props.env)).filter((v) => v.name !== named);
    await api.updateProjectEnv(props.project, [...kept, { name: named, fromSecret: secret.reference }]);
    using.value = null;
    toast.add({
      title: `${named} reads ${secret.name}`,
      description: "Variables land in the next release: what is running keeps its release's snapshot until the next deploy.",
      color: "success",
      icon: "i-lucide-check",
    });
    emit("saved");
  } catch (err) {
    using.value = null;
    writeError.value = err instanceof Error ? err.message : String(err);
  } finally {
    used.value = false;
  }
}

const removing = ref<ProjectSecret | null>(null);
const removed = ref(false);

async function remove() {
  const secret = removing.value;
  if (!secret || removed.value) return;
  removed.value = true;
  writeError.value = "";
  try {
    await api.deleteProjectSecret(props.project, secret.name);
    removing.value = null;
    await refresh();
    emit("saved");
  } catch (err) {
    removing.value = null;
    writeError.value = err instanceof Error ? err.message : String(err);
  } finally {
    removed.value = false;
  }
}
</script>

<template>
  <div class="space-y-4 max-w-3xl">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-sm font-medium text-highlighted">Secrets</h2>
        <p class="text-xs text-muted mt-1">
          Credentials <span class="font-mono">{{ project }}</span> needs that the platform did not create for it — a
          database it runs itself, an API key, an SMTP password. A value goes in and never comes back out: not to this
          dashboard, not to the API, not to anyone on the project. Replacing one means typing the new one.
        </p>
        <p class="text-xs text-dimmed mt-1">
          Point a variable at one to use it. Replacing the value then reaches what is already running, which a
          variable's own value does not — that lands in the next release.
        </p>
      </div>
      <UButton v-if="maySet" size="xs" color="neutral" variant="subtle" icon="i-lucide-plus" @click="openSet()">
        Add a secret
      </UButton>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <!-- Shown here when the write that failed was made from this panel, and
         inside the dialogue when it was made from there — a refusal belongs
         where the control that caused it is. -->
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
      <table class="w-full min-w-[34rem] text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default">
            <th class="px-3 py-2 font-medium">Name</th>
            <th class="px-3 py-2 font-medium">Read by</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!secrets.length">
            <td colspan="3" class="px-3 py-8 text-center text-muted">
              {{ loading ? "Loading…" : "No secrets. A credential this project needs goes here rather than in a variable's value." }}
            </td>
          </tr>
          <tr v-for="secret in secrets" :key="secret.name" class="border-b border-muted last:border-0">
            <td class="px-3 py-2">
              <p class="text-highlighted font-mono">{{ secret.name }}</p>
            </td>
            <td class="px-3 py-2">
              <template v-if="readersOf(secret).length">
                <UBadge
                  v-for="reader in readersOf(secret)"
                  :key="reader"
                  color="neutral"
                  variant="subtle"
                  size="sm"
                  class="font-mono mr-1"
                >
                  {{ reader }}
                </UBadge>
              </template>
              <!-- Nothing reads it yet, so the row says what to do about that
                   — and the reference underneath is what the CLI takes. -->
              <div v-else class="flex items-center gap-3">
                <UButton v-if="mayUse" color="neutral" variant="link" size="xs" class="px-0" @click="openUse(secret)">
                  Use in a variable
                </UButton>
                <span class="text-xs text-dimmed font-mono">
                  {{ secret.reference.name }}:{{ secret.reference.key }}
                </span>
              </div>
            </td>
            <td class="px-3 py-2 text-right whitespace-nowrap">
              <UButton
                v-if="maySet"
                color="neutral"
                variant="link"
                size="xs"
                class="px-0 mr-3"
                @click="openSet(secret)"
              >
                Replace
              </UButton>
              <UButton
                v-if="mayRemove"
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-trash-2"
                :aria-label="`Remove ${secret.name}`"
                @click="removing = secret"
              />
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <p v-if="!maySet && readOnlyReason" class="text-xs text-muted">{{ readOnlyReason }}.</p>

    <UModal
      v-model:open="open"
      :title="rotating ? `Replace ${rotating.name}` : 'Add a secret'"
      description="The value is stored where the application reads it and nowhere anything can read it back. It is not shown again."
      @update:open="(shown: boolean) => { if (!shown) close(); }"
    >
      <template #body>
        <form class="space-y-4" @submit.prevent="save">
          <UAlert v-if="writeError" color="warning" variant="soft" icon="i-lucide-info" :title="writeError" />
          <UFormField
            label="Name"
            help="How a variable refers to it. Letters, digits, and - _ . — a name like the variable that will read it reads best."
            :error="nameProblem || undefined"
          >
            <UInput
              v-model="name"
              :disabled="rotating !== null"
              placeholder="SMTP_PASSWORD"
              class="w-full font-mono"
              autocomplete="off"
            />
          </UFormField>
          <UFormField
            label="Value"
            :help="rotating ? 'The value that replaces the one stored now. There is nothing to prefill it with.' : 'Stored as written, with no trailing newline added.'"
          >
            <UInput
              v-model="value"
              type="password"
              placeholder="value"
              class="w-full font-mono"
              autocomplete="off"
            />
          </UFormField>
        </form>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="close">Cancel</UButton>
          <UButton
            :disabled="!name.trim() || !!nameProblem || !value"
            :loading="saving"
            icon="i-lucide-check"
            @click="save"
          >
            {{ rotating ? "Replace value" : "Set secret" }}
          </UButton>
        </div>
      </template>
    </UModal>

    <UModal
      :open="using !== null"
      :title="`Use ${using?.name ?? ''} in a variable`"
      description="The variable holds the reference, not the value — so the credential is never part of the project's configuration, and replacing it later changes nothing here."
      @update:open="(shown: boolean) => { if (!shown) using = null; }"
    >
      <template #body>
        <form class="space-y-4" @submit.prevent="use">
          <UFormField
            label="Variable name"
            help="What the application reads it as. A variable of this name that already exists is repointed at the secret, and drops whatever value it had."
          >
            <UInput v-model="variableName" class="w-full font-mono" autocomplete="off" />
          </UFormField>
        </form>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="using = null">Cancel</UButton>
          <UButton :disabled="!variableName.trim()" :loading="used" icon="i-lucide-check" @click="use">
            Add variable
          </UButton>
        </div>
      </template>
    </UModal>

    <UModal
      :open="removing !== null"
      :title="`Remove ${removing?.name ?? ''}?`"
      description="There is no way to read it back first. A variable that still points at it is refused, naming the variables to change first."
      @update:open="(shown: boolean) => { if (!shown) removing = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="removing = null">Cancel</UButton>
          <UButton color="error" :loading="removed" icon="i-lucide-trash-2" @click="remove">Remove secret</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
