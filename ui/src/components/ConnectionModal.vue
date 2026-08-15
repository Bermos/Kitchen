<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type Connection } from "../lib/api";

// The create flow the connections page used to point at kubectl for — and the
// edit flow that rotates a credential. The credential goes to the operator and
// never comes back: editing always starts from blank credential fields, and
// leaving them blank keeps what is stored.

const props = defineProps<{
  /** When set, the modal edits this connection instead of creating one. */
  connection?: Connection;
}>();
const emit = defineEmits<{ saved: [] }>();

const toast = useToast();
const open = ref(false);

const editing = computed(() => Boolean(props.connection));

const providers = [
  { label: "GitHub", value: "github" },
  { label: "GitLab", value: "gitlab" },
  { label: "Gitea", value: "gitea" },
  { label: "Container registry", value: "dockerRegistry" },
  { label: "Neon", value: "neon" },
];

const name = ref("");
const provider = ref("github");
const token = ref("");
const username = ref("");
const password = ref("");
// The one config field each provider understands today: the registry prefix,
// or a self-hosted GitHub's API URL.
const registryURL = ref("");
const apiURL = ref("");

const usesToken = computed(() => provider.value !== "dockerRegistry");

watch(open, (value) => {
  if (!value) return;
  // A fresh start on every open: prefill identity and config from the
  // connection being edited, never the credential.
  token.value = "";
  username.value = "";
  password.value = "";
  name.value = props.connection?.name ?? "";
  provider.value = props.connection?.provider ?? "github";
  registryURL.value = "";
  apiURL.value = "";
});

const credentialGiven = computed(() =>
  usesToken.value ? Boolean(token.value) : Boolean(username.value && password.value),
);

const ready = computed(() => {
  if (!editing.value && (!name.value || !provider.value)) return false;
  if (!editing.value && provider.value === "dockerRegistry" && !registryURL.value) return false;
  // Creating needs the credential; editing without one only changes config.
  if (!editing.value && !credentialGiven.value) return false;
  if (editing.value && !credentialGiven.value && !config.value) return false;
  return true;
});

const config = computed(() => {
  if (provider.value === "dockerRegistry" && registryURL.value) return { url: registryURL.value };
  if (provider.value === "github" && apiURL.value) return { apiUrl: apiURL.value };
  return undefined;
});

const credential = computed(() =>
  usesToken.value ? { token: token.value } : { username: username.value, password: password.value },
);

const saving = ref(false);
async function save() {
  if (!ready.value || saving.value) return;
  saving.value = true;
  try {
    if (editing.value) {
      await api.updateConnection(props.connection!.name, {
        config: config.value,
        credential: credentialGiven.value ? credential.value : undefined,
      });
      toast.add({
        title: credentialGiven.value
          ? `Credential for ${props.connection!.name} rotated`
          : `Connection ${props.connection!.name} updated`,
        color: "success",
        icon: "i-lucide-key-round",
      });
    } else {
      const created = await api.createConnection({
        name: name.value,
        provider: provider.value,
        config: config.value,
        credential: credential.value,
      });
      toast.add({ title: `Connection ${created.name} created`, color: "success", icon: "i-lucide-plug" });
    }
    open.value = false;
    emit("saved");
  } catch (err) {
    toast.add({
      title: editing.value ? "Updating the connection failed" : "Creating the connection failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <UModal
    v-model:open="open"
    :title="editing ? `Edit ${props.connection!.name}` : 'New connection'"
    :description="
      editing
        ? 'Rotate the credential or change the configuration. The stored credential is never shown; blank fields keep it.'
        : 'Link a git provider, registry or database. The credential is stored by the operator and never read back.'
    "
  >
    <slot>
      <UButton icon="i-lucide-plus" size="sm">New connection</UButton>
    </slot>

    <template #body>
      <form class="space-y-4" @submit.prevent="save">
        <div v-if="!editing" class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Name" help="Lowercase letters, digits and dashes." required>
            <UInput v-model="name" placeholder="gh" class="w-full font-mono" autofocus />
          </UFormField>
          <UFormField label="Provider" required>
            <USelect v-model="provider" :items="providers" class="w-full" />
          </UFormField>
        </div>

        <UFormField
          v-if="provider === 'dockerRegistry'"
          label="Registry"
          help="The prefix images are pushed under — builds authenticate against its host."
          :required="!editing"
        >
          <UInput v-model="registryURL" placeholder="harbor.example.com/kitchen" class="w-full font-mono" />
        </UFormField>
        <UFormField
          v-if="provider === 'github'"
          label="API URL"
          help="Only for GitHub Enterprise — leave empty for github.com."
        >
          <UInput v-model="apiURL" placeholder="https://github.internal/api/v3" class="w-full font-mono" />
        </UFormField>

        <UFormField
          v-if="usesToken"
          label="Token"
          :help="editing ? 'Leave empty to keep the stored token.' : 'An access token for the provider.'"
          :required="!editing"
        >
          <UInput v-model="token" type="password" class="w-full font-mono" autocomplete="off" />
        </UFormField>
        <div v-else class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Username" :required="!editing">
            <UInput v-model="username" class="w-full font-mono" autocomplete="off" />
          </UFormField>
          <UFormField
            label="Password"
            :help="editing ? 'Leave both empty to keep the stored credential.' : undefined"
            :required="!editing"
          >
            <UInput v-model="password" type="password" class="w-full font-mono" autocomplete="off" />
          </UFormField>
        </div>
      </form>
    </template>

    <template #footer>
      <div class="flex justify-end gap-2 w-full">
        <UButton color="neutral" variant="ghost" @click="open = false">Cancel</UButton>
        <UButton :disabled="!ready" :loading="saving" :icon="editing ? 'i-lucide-key-round' : 'i-lucide-plus'" @click="save">
          {{ editing ? "Save changes" : "Create connection" }}
        </UButton>
      </div>
    </template>
  </UModal>
</template>
