<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type NewClaim } from "../lib/api";
import { connectionChoices, noteFor, selectableChoices, type ConnectionChoice } from "../lib/connections";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";

// The create flow for resource claims: ask for something the project needs
// and let the platform provision it. Two kinds today — a database from a
// database-capable connection, and an OAuth client from the platform's own
// identity provider. The binding credentials stay in the cluster either way:
// the reconciler writes them into a secret the project's env vars reference,
// and nothing here ever sees them.

const props = defineProps<{ project: string }>();
const emit = defineEmits<{ saved: [] }>();

const toast = useToast();
const open = ref(false);

const name = ref("");
const connection = ref("");
const previewBranching = ref(false);
const deletionPolicy = ref("Retain");

// The two things the platform provisions. A database comes from a connection
// somebody configured; an OAuth client comes from the identity provider the
// platform already runs, which is why the second one asks for no connection.
const type = ref("postgres");
const typeOptions = [
  { label: "postgres — a database from a connection", value: "postgres" },
  { label: "oidcClient — single sign-on from the platform", value: "oidcClient" },
];
const isOIDC = computed(() => type.value === "oidcClient");

// The oidcClient half. All three are lists typed as free text, because all
// three are usually left alone: the defaults are what the help text says, and
// the operator keeps the generated redirect URIs in step by itself.
const callbackPaths = ref("");
const extraRedirectURIs = ref("");
const scopes = ref("");

/** A comma- or whitespace-separated list, as the API wants it. */
function entries(value: string): string[] {
  return value
    .split(/[\s,]+/)
    .map((entry) => entry.trim())
    .filter(Boolean);
}

const policyOptions = [
  { label: "Retain — keep the database when the claim is deleted", value: "Retain" },
  { label: "Delete — destroy the database and its data with the claim", value: "Delete" },
];

const connections = ref<ConnectionChoice[]>([]);
const connectionsLoaded = ref(false);

// Every connection is listed and the ones that cannot provision a database
// say so, on the same terms as the project's own two pickers — and read out
// of the same shape, which is the thinned one for anybody who is not an
// operator: a name, what it can back, and whether the platform has it
// working. A connection nothing has assessed yet is offered with the caveat
// rather than hidden; the reconciler holds the claim Pending until validation
// either way.
async function loadConnections() {
  connectionsLoaded.value = false;
  try {
    connections.value = connectionChoices(await api.connections(), "database");
  } catch {
    connections.value = [];
  } finally {
    connectionsLoaded.value = true;
  }
}

const connectionNote = computed(() => noteFor(connections.value, connection.value));
const available = computed(() => selectableChoices(connections.value));
const managesConnections = computed(() => may("POST /api/v1/connections", callerFor()));

watch(open, (value) => {
  if (!value) return;
  name.value = "";
  connection.value = "";
  previewBranching.value = false;
  deletionPolicy.value = "Retain";
  type.value = "postgres";
  callbackPaths.value = "";
  extraRedirectURIs.value = "";
  scopes.value = "";
  void loadConnections();
});

const ready = computed(() => Boolean(name.value && (isOIDC.value || connection.value)));

const saving = ref(false);
async function save() {
  if (!ready.value || saving.value) return;
  saving.value = true;
  try {
    // Each type sends its own fields and none of the other's: the API
    // refuses a request that mixes them, because a claim that quietly
    // ignored half of what it was asked for is worse than one that says so.
    const claim: NewClaim = isOIDC.value
      ? {
          name: name.value,
          project: props.project,
          connection: "",
          type: type.value,
          callbackPaths: entries(callbackPaths.value),
          redirectURIs: entries(extraRedirectURIs.value),
          scopes: entries(scopes.value),
        }
      : {
          name: name.value,
          project: props.project,
          connection: connection.value,
          type: type.value,
          previewBranching: previewBranching.value,
          deletionPolicy: deletionPolicy.value,
        };
    const created = await api.createClaim(claim);
    toast.add({
      title: `Claim ${created.name} created`,
      color: "success",
      icon: isOIDC.value ? "i-lucide-key-round" : "i-lucide-database",
    });
    open.value = false;
    emit("saved");
  } catch (err) {
    toast.add({
      title: "Creating the claim failed",
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
    title="New resource claim"
    :description="`Ask the platform to provision something for ${props.project}. The credentials land in a secret the project's env vars can reference — they are never shown here.`"
  >
    <slot>
      <UButton icon="i-lucide-plus" size="sm">New claim</UButton>
    </slot>

    <template #body>
      <form class="space-y-4" @submit.prevent="save">
        <div class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Name" help="Lowercase letters, digits and dashes." required>
            <UInput v-model="name" placeholder="shop-db" class="w-full font-mono" autofocus />
          </UFormField>
          <UFormField label="Type" required>
            <USelect v-model="type" :items="typeOptions" class="w-full" />
          </UFormField>
        </div>

        <template v-if="isOIDC">
          <p class="text-xs text-muted">
            The application signs its users in with the same accounts as this dashboard. The claim's secret holds
            <span class="font-mono">OIDC_ISSUER</span>, <span class="font-mono">CLIENT_ID</span> and
            <span class="font-mono">CLIENT_SECRET</span>; the platform keeps the client's redirect URIs in step with
            every environment of this project, so a preview's callback works the moment it is deployed and stops being
            accepted when the pull request closes.
          </p>

          <UFormField
            label="Callback paths"
            help="Appended to every environment URL. Empty registers /auth/callback and /api/auth/callback/kitchen."
          >
            <UInput v-model="callbackPaths" placeholder="/auth/callback" class="w-full font-mono" />
          </UFormField>

          <UFormField
            label="Extra redirect URIs"
            help="Registered verbatim, for addresses the platform does not own — a developer's localhost, typically."
          >
            <UInput
              v-model="extraRedirectURIs"
              placeholder="http://localhost:3000/auth/callback"
              class="w-full font-mono"
            />
          </UFormField>

          <UFormField label="Scopes" help="Empty asks for openid, profile, email and offline_access.">
            <UInput v-model="scopes" placeholder="openid profile email" class="w-full font-mono" />
          </UFormField>

          <p class="text-xs text-muted">
            Deleting this claim deregisters the client, and nothing can be signed in with it afterwards.
          </p>
        </template>

        <template v-else>
          <UFormField
            label="Connection"
            :help="connectionNote || 'A connection with the database capability provisions and owns the instance.'"
            required
          >
            <USelect
              v-model="connection"
              :items="connections"
              :placeholder="connectionsLoaded && !available.length ? 'No database-capable connections' : 'Select a connection'"
              :disabled="connectionsLoaded && !available.length"
              class="w-full"
            />
          </UFormField>
          <p v-if="connectionsLoaded && !available.length" class="text-xs text-muted">
            No connection can provision databases —
            <template v-if="managesConnections">create one first (e.g. a Neon connection) on the Connections page.</template>
            <template v-else>ask an operator to add one (e.g. a Neon connection).</template>
          </p>

          <USwitch
            v-model="previewBranching"
            label="Preview branching"
            description="Every preview environment gets its own database branch, created and torn down with the preview."
          />

          <UFormField
            label="On claim deletion"
            help="Retain is the default: deleting a claim must not be able to destroy a production database. Preview branches are always cleaned up."
          >
            <USelect v-model="deletionPolicy" :items="policyOptions" class="w-full" />
          </UFormField>
        </template>
      </form>
    </template>

    <template #footer>
      <div class="flex justify-end gap-2 w-full">
        <UButton color="neutral" variant="ghost" @click="open = false">Cancel</UButton>
        <UButton :disabled="!ready" :loading="saving" icon="i-lucide-plus" @click="save">Create claim</UButton>
      </div>
    </template>
  </UModal>
</template>
