<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, DATA_CLASSES, type ClaimProvider, type ClaimType, type Connection, type NewClaim } from "../lib/api";
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
// "" takes the provider's declared mode. What that is, and what the two
// alternatives mean, comes from the catalogue below — the point being that
// the developer sees "previews get a fresh, empty database" or "previews
// read and write production" at the moment they choose, not after.
const previewMode = ref("");
const deletionPolicy = ref("Retain");
// "" is unclassified. A class above the project's is refused by the API with
// the rule spelled out, so the select offers the vocabulary and lets the
// refusal teach the hierarchy.
const dataClass = ref("");
const dataClassOptions = [
  { label: "unclassified", value: "" },
  ...DATA_CLASSES.map((value) => ({ label: value, value: value as string })),
];

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

// What the database itself has to be. All four are optional and all four are
// applied when it is created: a major version is not something to change under
// a live Postgres, so asking for a different one is asking for a different
// database. An extension nothing can supply fails the claim with a message
// naming it, which is the whole point of asking here rather than finding out
// in a crash loop.
const pgVersion = ref("");
const pgExtensions = ref("");
const pgStorageSize = ref("");
const pgStorageClass = ref("");

// The majors CloudNativePG publishes images for, newest first. Empty takes the
// platform's own default, which is what most claims want.
const versionOptions = [
  { label: "the platform's default", value: "" },
  ...["18", "17", "16", "15", "14", "13"].map((major) => ({ label: major, value: major })),
];

/** The postgres block as the API takes it, or nothing when nothing was asked
 * for — an empty block on the claim would say the developer chose defaults
 * they never saw. */
function postgresRequest() {
  const extensions = entries(pgExtensions.value);
  const storage = {
    ...(pgStorageSize.value.trim() ? { size: pgStorageSize.value.trim() } : {}),
    ...(pgStorageClass.value.trim() ? { storageClass: pgStorageClass.value.trim() } : {}),
  };
  const postgres = {
    ...(pgVersion.value ? { version: pgVersion.value } : {}),
    ...(extensions.length ? { extensions } : {}),
    ...(Object.keys(storage).length ? { storage } : {}),
  };
  return Object.keys(postgres).length ? postgres : undefined;
}

const policyOptions = [
  { label: "Retain — keep the database when the claim is deleted", value: "Retain" },
  { label: "Delete — destroy the database and its data with the claim", value: "Delete" },
];

const connections = ref<ConnectionChoice[]>([]);
const connectionsLoaded = ref(false);
// The catalogue of what can be claimed and what each provider declares,
// and the provider behind each connection, so the declaration for the one
// chosen can be looked up.
const claimTypes = ref<ClaimType[]>([]);
const providerOf = ref<Record<string, string>>({});

async function loadClaimTypes() {
  try {
    claimTypes.value = await api.claimTypes();
  } catch {
    claimTypes.value = [];
  }
}

/** The declaration behind the selected connection (or the platform itself,
 * for a type that takes none). Undefined until both are known. */
const declaration = computed<ClaimProvider | undefined>(() => {
  const claimType = claimTypes.value.find((entry) => entry.type === type.value);
  if (!claimType) return undefined;
  if (!claimType.capability) return claimType.providers[0];
  const provider = providerOf.value[connection.value];
  return claimType.providers.find((entry) => entry.provider === provider);
});

/** Whether the type holds data — the case in which a shared preview writes
 * to production and has to be chosen by name. */
const holdsData = computed(() => claimTypes.value.find((entry) => entry.type === type.value)?.holdsData ?? true);

const PREVIEW_LABELS: Record<string, string> = {
  branch: "a branch of production's data — cheap, and production-derived",
  fresh: "a fresh, empty resource of its own — never a copy of production",
  shared: "production itself — previews read and write what production does",
  none: "nothing — the variables that read this claim are left out of previews",
};

/** What previews get, as the options of the picker: the provider's own
 * mode first and preselected, then shared and none, each saying what it
 * means. A provider whose own mode is shared for a data-holding type
 * preselects nothing: that choice is made by name or not at all. */
const previewOptions = computed(() => {
  const declared = declaration.value;
  if (!declared) return [];
  return declared.previewChoices.map((mode) => ({
    label: `${mode} — ${mode === declared.previewMode ? declared.previewNote : PREVIEW_LABELS[mode] ?? mode}`,
    value: mode,
  }));
});

watch(declaration, (declared) => {
  if (!declared) return;
  const defaultMode = declared.previewMode === "shared" && holdsData.value ? "none" : declared.previewMode;
  if (!declared.previewChoices.includes(previewMode.value)) previewMode.value = defaultMode;
});

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
    const all: Connection[] = await api.connections();
    connections.value = connectionChoices(all, "database");
    providerOf.value = Object.fromEntries(all.map((entry) => [entry.name, entry.provider ?? ""]));
  } catch {
    connections.value = [];
    providerOf.value = {};
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
  previewMode.value = "";
  deletionPolicy.value = "Retain";
  dataClass.value = "";
  type.value = "postgres";
  callbackPaths.value = "";
  extraRedirectURIs.value = "";
  scopes.value = "";
  pgVersion.value = "";
  pgExtensions.value = "";
  pgStorageSize.value = "";
  pgStorageClass.value = "";
  void loadConnections();
  void loadClaimTypes();
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
          ...(dataClass.value ? { dataClass: dataClass.value } : {}),
        }
      : {
          name: name.value,
          project: props.project,
          connection: connection.value,
          type: type.value,
          ...(previewMode.value ? { previewMode: previewMode.value } : {}),
          deletionPolicy: deletionPolicy.value,
          ...(postgresRequest() ? { postgres: postgresRequest() } : {}),
          ...(dataClass.value ? { dataClass: dataClass.value } : {}),
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
            <template v-if="managesConnections">
              create one first on the Connections page — CloudNativePG for a database the platform runs itself and
              needs no account anywhere, or Neon for a hosted one.
            </template>
            <template v-else>
              ask an operator to add one — CloudNativePG for a database the platform runs itself, or Neon for a
              hosted one.
            </template>
          </p>

          <UFormField
            label="Previews get"
            :help="
              declaration
                ? `What this connection's provider declares a preview environment gets. Shared is never a default: it has to be chosen here.`
                : 'Choose a connection to see what its provider gives a preview environment.'
            "
          >
            <USelect
              v-model="previewMode"
              :items="previewOptions"
              :disabled="!previewOptions.length"
              placeholder="the provider's own declaration"
              class="w-full"
            />
          </UFormField>
          <UAlert
            v-if="previewMode === 'shared' && holdsData"
            color="warning"
            variant="subtle"
            icon="i-lucide-triangle-alert"
            title="Previews will read and write production's data"
            description="Every preview environment of this project binds to the production resource itself. Nothing isolates a pull request's changes from production."
          />
          <UAlert
            v-if="declaration?.keepsPodsRunning || declaration?.forcesRecreate"
            color="warning"
            variant="subtle"
            icon="i-lucide-triangle-alert"
            :title="
              declaration?.keepsPodsRunning
                ? 'Environments reading this claim will not scale to zero'
                : 'Environments reading this claim will have downtime on every deploy'
            "
            :description="declaration?.workloadNote"
          />

          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Postgres version" help="Empty takes the platform's default.">
              <USelect v-model="pgVersion" :items="versionOptions" class="w-full" />
            </UFormField>
            <UFormField
              label="Extensions"
              help="Created in the database when it is built, so the application never has to. postgis, vector, pg_trgm."
            >
              <UInput v-model="pgExtensions" placeholder="postgis vector" class="w-full font-mono" />
            </UFormField>
          </div>
          <p class="text-xs text-muted">
            A version or an extension the connection cannot supply fails the claim with a message saying what is
            available — rather than binding and letting the application die on a
            <span class="font-mono">CREATE EXTENSION</span> later. A connection to a hosted Postgres cannot be asked
            for either, and says so.
          </p>

          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Storage" help="A Kubernetes quantity. Empty takes the platform's default.">
              <UInput v-model="pgStorageSize" placeholder="10Gi" class="w-full font-mono" />
            </UFormField>
            <UFormField label="Storage class" help="Empty takes the platform's default.">
              <UInput v-model="pgStorageClass" placeholder="fast-ssd" class="w-full font-mono" />
            </UFormField>
          </div>
          <p class="text-xs text-muted">
            All four are applied when the database is created. Changing them afterwards asks for a different
            database rather than reshaping this one.
          </p>

          <UFormField
            label="On claim deletion"
            help="Retain is the default: deleting a claim must not be able to destroy a production database. Preview branches are always cleaned up."
          >
            <USelect v-model="deletionPolicy" :items="policyOptions" class="w-full" />
          </UFormField>
        </template>

        <UFormField
          label="Data classification"
          help="What class of data the resource will hold. It narrows the project's class and may not exceed it — classify the project first if it has none."
        >
          <USelect v-model="dataClass" :items="dataClassOptions" class="w-full" />
        </UFormField>
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
