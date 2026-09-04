<script setup lang="ts">
import { computed, ref, watch } from "vue";
import {
  api,
  DATA_CLASSES,
  type BindableVolume,
  type BindableVolumes,
  type ClaimProvider,
  type ClaimType,
  type Connection,
  type NewClaim,
} from "../lib/api";
import OperatorOnly from "./OperatorOnly.vue";
import { DESTRUCTIVE_POLICY, destroysDataRefusal, mayDestroyData } from "../lib/claims";
import { connectionChoices, noteFor, selectableChoices, type ConnectionChoice } from "../lib/connections";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";

// The create flow for resource claims: ask for something the project needs
// and let the platform provision it. Five kinds today — a database from a
// database-capable connection, a bucket from an object store, an OAuth client
// from the platform's own identity provider, a persistent volume mounted into
// one of the project's processes, and durable background work from an Inngest
// account behind a connection. The binding credentials stay in the cluster in
// every case: the reconciler writes them into a secret the project's env vars
// reference, and nothing here ever sees them. The volume is the exception —
// it produces a mount rather than a credential, and the one thing it has to
// say up front is what it costs the process that mounts it.

const props = defineProps<{
  project: string;
  /** The caller's role on this project, as it arrived on the project's own
   * payload. It decides one thing here: `deletionPolicy: Delete` destroys
   * the provisioned data and is the admin's, which the API enforces at the
   * door — so the option is not offered to a developer as if it were. */
  role?: string;
  /** The names of the project's declared processes, for the volume picker.
   * The web process is implicit and always offered. */
  processes?: string[];
}>();
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

// The four things the platform provisions. A database and a bucket come from
// a connection somebody configured; an OAuth client comes from the identity
// provider the platform already runs; a volume comes from the cluster's own
// storage — which is why the last two ask for no connection.
// The four things the platform provisions. A database, a bucket and an
// Inngest app come from a connection somebody configured; an OAuth client
// comes from the identity provider the platform already runs, which is why
// that one asks for no connection.
const type = ref("postgres");
const typeOptions = [
  { label: "postgres — a database from a connection", value: "postgres" },
  { label: "objectStore — a bucket from a connection", value: "objectStore" },
  { label: "oidcClient — single sign-on from the platform", value: "oidcClient" },
  { label: "volume — a persistent disk mounted into one process, cut new or already there", value: "volume" },
  { label: "inngest — durable background work from Inngest Cloud", value: "inngest" },
  { label: "redis — a cache or a queue from a connection", value: "redis" },
];
const isOIDC = computed(() => type.value === "oidcClient");
const isPostgres = computed(() => type.value === "postgres");
const isObjectStore = computed(() => type.value === "objectStore");

// The objectStore half: what the bucket has to be. All three are applied
// when the bucket is created, and a store that cannot honour one — public
// reads at the bundled store, which nothing outside the cluster reaches —
// fails the claim with the reason rather than granting a policy that
// publishes nothing.
const bucketVersioning = ref(false);
const bucketPublicRead = ref(false);
const bucketSize = ref("");

/** The objectStore block as the API takes it, or nothing when nothing was
 * asked for. */
function objectStoreRequest() {
  const block = {
    ...(bucketVersioning.value ? { versioning: true } : {}),
    ...(bucketPublicRead.value ? { publicRead: true } : {}),
    ...(bucketSize.value.trim() ? { size: bucketSize.value.trim() } : {}),
  };
  return Object.keys(block).length ? block : undefined;
}
const isVolume = computed(() => type.value === "volume");

const isInngest = computed(() => type.value === "inngest");
const isRedis = computed(() => type.value === "redis");

// The redis half. `usage` is the only one that is really a choice: a cache
// may evict what it holds when it fills up and a queue may not, and an
// application handed the wrong one loses work without being told. It is
// asked here rather than defaulted quietly for that reason.
const redisUsage = ref("cache");
const redisMaxMemory = ref("");
const redisVersion = ref("");

const redisUsageOptions = [
  { label: "cache — may evict what it holds when it fills up", value: "cache" },
  { label: "queue — must not; the write fails instead", value: "queue" },
];

/** The redis block as the API takes it. `usage` always goes, because the
 * developer chose it here rather than inheriting a default they never saw. */
function redisRequest() {
  return {
    usage: redisUsage.value,
    ...(redisMaxMemory.value.trim() ? { maxMemory: redisMaxMemory.value.trim() } : {}),
    ...(redisVersion.value.trim() ? { version: redisVersion.value.trim() } : {}),
  };
}

// The inngest half. The app ID is the one thing the application has to
// match — its Inngest client is created with it — and the environment is
// where production's events go; previews get a branch environment each
// whatever is written here. Connect is the only mode, and it is not asked.
const inngestApp = ref("");
const inngestEnvironment = ref("");

/** The inngest block as the API takes it, or nothing when every default is
 * taken. */
function inngestRequest() {
  const inngest = {
    ...(inngestApp.value.trim() ? { app: inngestApp.value.trim() } : {}),
    ...(inngestEnvironment.value.trim() ? { environment: inngestEnvironment.value.trim() } : {}),
  };
  return Object.keys(inngest).length ? inngest : undefined;
}

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

// The volume half: which process mounts it, where, and what it has to be.
// The process is the one thing a volume claim cannot go without — a volume
// attaches to one copy of a process at a time, and every process of the
// environment would otherwise want it.
const volProcess = ref("web");
const volMountPath = ref("");
const volSize = ref("");
const volStorageClass = ref("");
const processOptions = computed(() => [
  { label: "web — the web process", value: "web" },
  ...(props.processes ?? []).map((process) => ({ label: process, value: process })),
]);

// The other half of what a persistent workload is: the data that was already
// there. "provision" cuts a new disk, "bind" mounts one that existed before
// the cluster did — a share on the network storage, something somebody
// attached by hand. It is asked here rather than worked out from which
// fields were filled in, because the difference between cutting a disk and
// reaching for twelve terabytes somebody already has is not a thing to
// guess at, and the API refuses each source the other's fields.
const VOLUME_PROVIDERS: Record<string, string> = { provision: "storageClass", bind: "boundVolume" };
const volSource = ref("provision");
const isBoundVolume = computed(() => isVolume.value && volSource.value === "bind");
const volSourceOptions = [
  { label: "provision — cut a new, empty disk for this project", value: "provision" },
  { label: "bind — mount storage that already exists, and holds data", value: "bind" },
];

// What is actually there to bind, read when the form opens. A bound volume
// is *named*, and a name typed from memory is the failure this list removes.
const bindable = ref<BindableVolumes>({ persistentVolumes: [], persistentVolumeClaims: [] });
const bindableLoaded = ref(false);
// "volume:<name>" or "claim:<name>" — the two vocabularies kept apart, since
// one is the storage itself and the other is this project's own handle on it.
const volBind = ref("");
const volAccessMode = ref("ReadOnlyMany");

async function loadBindableVolumes() {
  bindableLoaded.value = false;
  try {
    bindable.value = await api.claimVolumes();
  } catch {
    bindable.value = { persistentVolumes: [], persistentVolumeClaims: [] };
  } finally {
    bindableLoaded.value = true;
  }
}

/** Every volume that can be bound, with what it holds and — where one cannot
 * be — why. A volume nothing can mount is listed disabled rather than left
 * out: a name somebody was told to use must not simply fail to appear. */
const bindOptions = computed(() => [
  ...bindable.value.persistentVolumes.map((entry) => ({
    label: [entry.name, entry.capacity, entry.note ?? (entry.heldBy?.length ? `shared with ${entry.heldBy.join(", ")}` : "")]
      .filter(Boolean)
      .join(" — "),
    value: `volume:${entry.name}`,
    disabled: !entry.readable && !entry.writable,
  })),
  ...bindable.value.persistentVolumeClaims
    .filter((entry) => entry.project === props.project)
    .map((entry) => ({
      label: [entry.name, entry.capacity, entry.managedByKitchen ? "made by the platform for another claim" : ""]
        .filter(Boolean)
        .join(" — "),
      value: `claim:${entry.name}`,
      disabled: false,
    })),
]);

/** The volume the picker is on, where it is one of the cluster's own. */
const chosenVolume = computed<BindableVolume | undefined>(() =>
  bindable.value.persistentVolumes.find((entry) => `volume:${entry.name}` === volBind.value),
);

/** How this project may mount what it chose. Read-only is always offered
 * where the volume can be read at all — any number of projects may hold one
 * volume that way. Writing is offered only where nothing else is already
 * writing the same storage: two projects writing one filesystem is one
 * project's deploy breaking another's, and the API refuses the second. */
const accessModeOptions = computed(() => {
  const chosen = chosenVolume.value;
  const offers = chosen?.accessModes ?? ["ReadOnlyMany", "ReadWriteOnce", "ReadWriteMany"];
  const options = [
    {
      label: "ReadOnlyMany — read it; every copy of the process, and every preview, may",
      value: "ReadOnlyMany",
      disabled: chosen ? !chosen.readable : false,
    },
    {
      label: "ReadWriteOnce — write it, as the only copy that has it",
      value: "ReadWriteOnce",
      disabled: chosen ? !chosen.writable || !offers.includes("ReadWriteOnce") : false,
    },
    {
      label: "ReadWriteMany — write it alongside other copies",
      value: "ReadWriteMany",
      disabled: chosen ? !chosen.writable || !offers.includes("ReadWriteMany") : false,
    },
  ];
  return options;
});

// A mode that has just become unavailable — the volume changed under the
// picker — is let go rather than submitted and refused.
watch(accessModeOptions, (options) => {
  if (options.find((option) => option.value === volAccessMode.value)?.disabled) {
    volAccessMode.value = options.find((option) => !option.disabled)?.value ?? "ReadOnlyMany";
  }
});

/** What this claim costs the process that mounts it, in the one sentence
 * that differs between the two sources. A volume attached to one pod at a
 * time caps the process at one copy and puts a gap in every deploy;
 * ReadOnlyMany and ReadWriteMany both lift it. */
const attachesOnce = computed(() => !isBoundVolume.value || volAccessMode.value === "ReadWriteOnce");

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

/** The resource noun the claim provisions — from the catalogue, so the
 * policy picker and the empty states name the right thing for every type. */
const resourceNoun = computed(() => claimTypes.value.find((entry) => entry.type === type.value)?.resource ?? "resource");

/** Whether this caller may ask for the policy that destroys the data, and the
 * sentence the form says when they may not. Destroying data is the admin's
 * (#320); the rest of the claim is the developer's day job. */
const mayDestroy = computed(() => mayDestroyData(callerFor(props.role, props.project)));
const destroyRefusal = computed(() =>
  destroysDataRefusal(callerFor(props.role, props.project), `asking for a claim that destroys its ${resourceNoun.value}`),
);

const policyOptions = computed(() => [
  { label: `Retain — keep the ${resourceNoun.value} when the claim is deleted`, value: "Retain" },
  {
    label: isBoundVolume.value
      ? `Delete — refused here: this ${resourceNoun.value} is not the platform's to destroy`
      : `Delete — destroy the ${resourceNoun.value} and its data with the claim`,
    value: DESTRUCTIVE_POLICY,
    // The volume existed before the claim and the platform neither made it
    // nor owns it, so there is no policy that may destroy it — deleting the
    // claim unmounts it and leaves every byte where it is. The API refuses
    // the request; the form does not offer it.
    disabled: !mayDestroy.value || isBoundVolume.value,
  },
]);

// A developer who had the option open and lost it — the role arriving after
// the form did, or the source moving to one where nothing may be destroyed —
// must not be left holding a choice the API will refuse.
watch([mayDestroy, isBoundVolume], () => {
  if ((!mayDestroy.value || isBoundVolume.value) && deletionPolicy.value === DESTRUCTIVE_POLICY) {
    deletionPolicy.value = "Retain";
  }
});

/** The volume block as the API takes it. Each source sends its own fields
 * and none of the other's: the API refuses a size on a volume it is not
 * about to cut, because a number nothing reads reads as a disk about to be
 * made at it. */
function volumeRequest() {
  const common = { process: volProcess.value, mountPath: volMountPath.value.trim() };
  if (!isBoundVolume.value) {
    return {
      ...common,
      source: "provision",
      size: volSize.value.trim(),
      ...(volStorageClass.value.trim() ? { storageClass: volStorageClass.value.trim() } : {}),
    };
  }
  const [kind, target] = [volBind.value.slice(0, volBind.value.indexOf(":")), volBind.value.slice(volBind.value.indexOf(":") + 1)];
  return {
    ...common,
    source: "bind",
    bind: {
      ...(kind === "claim" ? { persistentVolumeClaim: target } : { persistentVolume: target }),
      accessMode: volAccessMode.value,
    },
  };
}

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
  // A volume has two providers and the claim's own source picks between
  // them, since nothing else can: cutting a disk and mounting one somebody
  // already has declare opposite things about previews, and reading the
  // first would judge one against the other's rules.
  if (isVolume.value) {
    return claimType.providers.find((entry) => entry.provider === VOLUME_PROVIDERS[volSource.value]);
  }
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
  return declared.previewChoices
    // A volume mounted as the only copy that has it is production's while
    // production is running, so a preview sharing it would take it away.
    // The API refuses the choice; the form does not offer it.
    .filter((mode) => !(mode === "shared" && isBoundVolume.value && attachesOnce.value))
    .map((mode) => ({
      label: `${mode} — ${mode === declared.previewMode ? declared.previewNote : PREVIEW_LABELS[mode] ?? mode}`,
      value: mode,
    }));
});

watch([declaration, previewOptions], () => {
  const declared = declaration.value;
  if (!declared) return;
  // A provider that declares shared for a type that holds data preselects
  // nothing — that choice is made by name or not at all — unless what it
  // shares cannot be written, which takes nothing from production.
  const shareIsATaking = declared.previewMode === "shared" && holdsData.value && !declared.sharedIsReadOnly;
  const offered = previewOptions.value.map((option) => option.value);
  const defaultMode = shareIsATaking || !offered.includes(declared.previewMode) ? "none" : declared.previewMode;
  if (!offered.includes(previewMode.value)) previewMode.value = defaultMode;
});

// Every connection is listed and the ones that cannot provision the chosen
// type say so, on the same terms as the project's own two pickers — and read
// out of the same shape, which is the thinned one for anybody who is not an
// operator: a name, what it can back, and whether the platform has it
// working. A connection nothing has assessed yet is offered with the caveat
// rather than hidden; the reconciler holds the claim Pending until validation
// either way. The capability the type needs comes from the catalogue, so a
// new type never has to be spelled here.
const allConnections = ref<Connection[]>([]);
const capability = computed(
  () => claimTypes.value.find((entry) => entry.type === type.value)?.capability ?? (isObjectStore.value ? "objectStore" : "database"),
);
const connections = computed<ConnectionChoice[]>(() => connectionChoices(allConnections.value, capability.value));

async function loadConnections() {
  connectionsLoaded.value = false;
  try {
    const all: Connection[] = await api.connections();
    allConnections.value = all;
    providerOf.value = Object.fromEntries(all.map((entry) => [entry.name, entry.provider ?? ""]));
  } catch {
    allConnections.value = [];
    providerOf.value = {};
  } finally {
    connectionsLoaded.value = true;
  }
}
// The choices are re-read against the capability whenever the type moves —
// a connection that provisions databases is the wrong one for an Inngest app,
// and says so — and a connection that has just become the wrong one is let
// go rather than submitted and refused.
watch(connections, (choices) => {
  if (connection.value && choices.find((entry) => entry.value === connection.value)?.disabled) {
    connection.value = "";
  }
});

// A connection chosen for one type is not necessarily able to provision the
// next, so changing the type clears the choice rather than carrying it over.
watch(type, () => {
  connection.value = "";
});

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
  bucketVersioning.value = false;
  bucketPublicRead.value = false;
  bucketSize.value = "";
  volProcess.value = "web";
  volMountPath.value = "";
  volSize.value = "";
  volStorageClass.value = "";
  volSource.value = "provision";
  volBind.value = "";
  volAccessMode.value = "ReadOnlyMany";
  redisUsage.value = "cache";
  redisMaxMemory.value = "";
  redisVersion.value = "";
  inngestApp.value = "";
  inngestEnvironment.value = "";
  bindable.value = { persistentVolumes: [], persistentVolumeClaims: [] };
  bindableLoaded.value = false;
  void loadConnections();
  void loadClaimTypes();
});

// What is there to bind is read the moment somebody asks to bind something,
// and not before: it is a read across the cluster's storage, and the four
// claim types that never bind anything have no use for it.
watch(isBoundVolume, (bound) => {
  if (bound && !bindableLoaded.value) void loadBindableVolumes();
});

const ready = computed(() => {
  if (!name.value) return false;
  if (isOIDC.value) return true;
  if (isBoundVolume.value) {
    return Boolean(volProcess.value && volMountPath.value.trim() && volBind.value && volAccessMode.value);
  }
  if (isVolume.value) return Boolean(volProcess.value && volSize.value.trim() && volMountPath.value.trim());
  return Boolean(connection.value);
});

const saving = ref(false);
async function save() {
  if (!ready.value || saving.value) return;
  saving.value = true;
  try {
    // Each type sends its own fields and none of the others': the API
    // refuses a request that mixes them, because a claim that quietly
    // ignored half of what it was asked for is worse than one that says so.
    let claim: NewClaim;
    if (isOIDC.value) {
      claim = {
        name: name.value,
        project: props.project,
        connection: "",
        type: type.value,
        callbackPaths: entries(callbackPaths.value),
        redirectURIs: entries(extraRedirectURIs.value),
        scopes: entries(scopes.value),
        ...(dataClass.value ? { dataClass: dataClass.value } : {}),
      };
    } else if (isVolume.value) {
      claim = {
        name: name.value,
        project: props.project,
        connection: "",
        type: type.value,
        volume: volumeRequest(),
        ...(previewMode.value ? { previewMode: previewMode.value } : {}),
        deletionPolicy: deletionPolicy.value,
        ...(dataClass.value ? { dataClass: dataClass.value } : {}),
      };
    } else if (isInngest.value) {
      claim = {
        name: name.value,
        project: props.project,
        connection: connection.value,
        type: type.value,
        ...(previewMode.value ? { previewMode: previewMode.value } : {}),
        ...(inngestRequest() ? { inngest: inngestRequest() } : {}),
        ...(dataClass.value ? { dataClass: dataClass.value } : {}),
      };
    } else {
      claim = {
        name: name.value,
        project: props.project,
        connection: connection.value,
        type: type.value,
        ...(previewMode.value ? { previewMode: previewMode.value } : {}),
        deletionPolicy: deletionPolicy.value,
        ...(isPostgres.value && postgresRequest() ? { postgres: postgresRequest() } : {}),
        ...(isObjectStore.value && objectStoreRequest() ? { objectStore: objectStoreRequest() } : {}),
        ...(isRedis.value ? { redis: redisRequest() } : {}),
        ...(dataClass.value ? { dataClass: dataClass.value } : {}),
      };
    }
    const created = await api.createClaim(claim);
    toast.add({
      title: `Claim ${created.name} created`,
      color: "success",
      icon: isOIDC.value
        ? "i-lucide-key-round"
        : isObjectStore.value
          ? "i-lucide-folder-archive"
          : isVolume.value
            ? "i-lucide-hard-drive"
          : isInngest.value
            ? "i-lucide-workflow"
            : isRedis.value
              ? "i-lucide-zap"
              : "i-lucide-database",
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
    :description="`Ask the platform to provision something for ${props.project}. Credentials land in a secret the project's env vars can reference — they are never shown here; a volume lands as a mount.`"
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

        <template v-else-if="isVolume">
          <p class="text-xs text-muted">
            A persistent disk for an application that writes where it was told to write — a legacy service, SQLite,
            anything that keeps its state on a filesystem. It is mounted into exactly one of this project's processes,
            and it survives every deploy and restart of that process.
          </p>

          <UFormField
            label="Where the storage comes from"
            help="Provision cuts a new, empty disk. Bind mounts storage that already exists and already holds
              data — a share on the network storage, something attached by hand before this platform was installed.
              Binding provisions nothing and destroys nothing."
          >
            <USelect v-model="volSource" :items="volSourceOptions" class="w-full" />
          </UFormField>

          <!-- The cost, stated where the claim is made and not only in the
               docs: the same sentence the "Never run two at once" switch
               says about a singleton, because it is the same trade. It is
               the access mode that decides it, so a volume read by many
               copies at once does not say it. -->
          <UAlert
            v-if="attachesOnce"
            color="warning"
            variant="subtle"
            icon="i-lucide-triangle-alert"
            title="Deploys of this process will have a gap in serving, and it runs one copy"
            description="A volume can be attached to one copy of a process at a time. Deploys stop the old copy before
              starting the new one — a rolling deploy would leave the new copy waiting for a disk the old one never
              lets go of — so there is a gap in serving on every deploy, and the replica count of that process is fixed
              at 1. Storage the platform finds to support shared access lifts both, and the claim says which
              it found."
          />

          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Process" help="The one process that mounts the volume." required>
              <USelect v-model="volProcess" :items="processOptions" class="w-full" />
            </UFormField>
            <UFormField label="Mount path" help="Where the volume appears inside the process." required>
              <UInput v-model="volMountPath" placeholder="/data" class="w-full font-mono" />
            </UFormField>
          </div>

          <div v-if="!isBoundVolume" class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Size" help="A quantity such as 10Gi. Set when the volume is created; it is not shrunk." required>
              <UInput v-model="volSize" placeholder="10Gi" class="w-full font-mono" />
            </UFormField>
            <UFormField label="Storage class" help="Empty takes the platform's default.">
              <UInput v-model="volStorageClass" placeholder="fast-ssd" class="w-full font-mono" />
            </UFormField>
          </div>

          <template v-else>
            <UFormField
              label="Which storage"
              help="What the platform can already see. Storage nothing may mount is listed with the reason rather
                than left out, so a name somebody gave you never simply fails to appear."
              required
            >
              <USelect
                v-model="volBind"
                :items="bindOptions"
                :disabled="!bindOptions.length"
                :placeholder="bindableLoaded && !bindOptions.length ? 'There is none to bind' : 'Select the storage to mount'"
                class="w-full"
              />
              <OperatorOnly>
                <p v-if="chosenVolume" class="mt-1 text-xs text-muted">
                  PersistentVolume {{ chosenVolume.name }}<template v-if="chosenVolume.storageClass">, storageClass
                  {{ chosenVolume.storageClass }}</template><template v-if="chosenVolume.identity">, {{ chosenVolume.identity }}</template
                  ><template v-if="chosenVolume.phase">, {{ chosenVolume.phase }}</template>. A PersistentVolumeClaim
                  of this project's own namespace is bound by name instead; one of another project's cannot be, because
                  a pod only mounts its own.
                </p>
              </OperatorOnly>
            </UFormField>

            <UFormField
              label="This project mounts it"
              help="Declared, not read off the storage: what the storage can do and what this project may do with it
                are different questions, and only the second decides whether another project may write it. Any number
                of projects may read one volume; exactly one may write it, and the second writer is refused naming
                the first."
              required
            >
              <USelect v-model="volAccessMode" :items="accessModeOptions" class="w-full" />
              <p v-if="chosenVolume?.note" class="mt-1 text-xs text-muted">{{ chosenVolume.note }}.</p>
              <p v-else-if="chosenVolume?.heldBy?.length" class="mt-1 text-xs text-muted">
                Already held by {{ chosenVolume.heldBy.join(", ") }}.
              </p>
            </UFormField>
          </template>

          <UFormField
            label="Previews get"
            :help="
              isBoundVolume
                ? attachesOnce
                  ? 'Nothing: this storage attaches to one copy at a time and production has it, so a preview mounting it would take it away.'
                  : 'The same storage, read-only — a preview of an application whose data is the point of it reads exactly what production reads, and can change none of it.'
                : 'A fresh, empty volume of the same size for each preview, torn down with it — never production\'s own, which the process could not share.'
            "
          >
            <USelect
              v-model="previewMode"
              :items="previewOptions"
              :disabled="!previewOptions.length"
              placeholder="the platform's own declaration"
              class="w-full"
            />
          </UFormField>

          <UFormField
            label="On claim deletion"
            :help="
              isBoundVolume
                ? 'There is no choice here: this storage existed before the claim and the platform neither made it nor owns it, so deleting the claim unmounts it and leaves every byte where it is.'
                : 'Retain is the default: deleting a claim must not be able to destroy the data on a production volume. A retained volume outlives the project, and a claim of the same name binds to it again. Preview volumes are always cleaned up.'
            "
          >
            <USelect v-model="deletionPolicy" :items="policyOptions" class="w-full" />
            <p v-if="destroyRefusal && !isBoundVolume" class="mt-1 text-xs text-muted">{{ destroyRefusal }}.</p>
          </UFormField>
        </template>

        <template v-else>
          <UFormField
            label="Connection"
            :help="connectionNote || `A connection with the ${capability} capability provisions and owns the ${resourceNoun}.`"
            required
          >
            <USelect
              v-model="connection"
              :items="connections"
              :placeholder="connectionsLoaded && !available.length ? `No ${capability}-capable connections` : 'Select a connection'"
              :disabled="connectionsLoaded && !available.length"
              class="w-full"
            />
          </UFormField>
          <p v-if="connectionsLoaded && !available.length && isInngest" class="text-xs text-muted">
            No connection can reach an Inngest account —
            <template v-if="managesConnections">
              create an Inngest Cloud connection first on the Connections page, with an API key from the Inngest
              dashboard.
            </template>
            <template v-else>ask an operator to add an Inngest Cloud connection.</template>
          </p>
          <p v-else-if="connectionsLoaded && !available.length && isPostgres" class="text-xs text-muted">
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
          <p v-if="connectionsLoaded && !available.length && isObjectStore" class="text-xs text-muted">
            No connection can provision buckets —
            <template v-if="managesConnections">
              create an S3-compatible connection first on the Connections page, or switch the bundled store on in
              the chart (objectStore.enabled), which seeds one.
            </template>
            <template v-else>
              ask an operator to add an S3-compatible connection, or to switch the bundled store on.
            </template>
          </p>

          <template v-if="isInngest">
            <p class="text-xs text-muted">
              The claim's secret holds <span class="font-mono">INNGEST_EVENT_KEY</span>,
              <span class="font-mono">INNGEST_SIGNING_KEY</span>, <span class="font-mono">INNGEST_ENV</span> and
              <span class="font-mono">INNGEST_BASE_URL</span>, read from the Inngest account — the platform creates
              no keys, because Inngest's API cannot. The process holding the worker connects out to Inngest with
              them; in a preview, <span class="font-mono">INNGEST_ENV</span> names a branch environment of the
              preview's own. Reference them from the project's environment variables.
            </p>
            <div class="grid gap-4 sm:grid-cols-2">
              <UFormField
                label="App ID"
                help="The id the application's Inngest client is created with. Empty takes the claim's name."
              >
                <UInput v-model="inngestApp" placeholder="shop-worker" class="w-full font-mono" />
              </UFormField>
              <UFormField
                label="Inngest environment"
                help="Where production's events go: production, or a custom environment from the Inngest dashboard. Previews get branch environments regardless."
              >
                <UInput v-model="inngestEnvironment" placeholder="production" class="w-full font-mono" />
              </UFormField>
            </div>
            <p class="text-xs text-muted">
              Deleting this claim removes the binding and archives the preview branch environments; the app and
              the keys stay at Inngest. The environment's event key has to exist already — a claim against an
              environment without one fails saying where to create it.
            </p>
          </template>

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

          <template v-if="isRedis">
            <UFormField
              label="What it is for"
              help="A cache may evict what it holds when it fills up; a queue may not — the write fails instead, where the application can retry."
              required
            >
              <USelect v-model="redisUsage" :items="redisUsageOptions" class="w-full" />
            </UFormField>
            <UAlert
              v-if="redisUsage === 'queue'"
              color="neutral"
              variant="subtle"
              icon="i-lucide-shield-check"
              title="This instance will refuse writes rather than drop work"
              description="A queue keeps what is in it on disk and stops accepting writes when it is full. That is the point: a queue that evicts loses jobs and reports nothing."
            />
            <div class="grid gap-4 sm:grid-cols-2">
              <UFormField label="Max memory" help="A Kubernetes quantity. Empty takes the platform's default.">
                <UInput v-model="redisMaxMemory" placeholder="512Mi" class="w-full font-mono" />
              </UFormField>
              <UFormField label="Valkey version" help="A major version. Empty takes the platform's default.">
                <UInput v-model="redisVersion" placeholder="8" class="w-full font-mono" />
              </UFormField>
            </div>
            <p class="text-xs text-muted">
              The secret carries <span class="font-mono">url</span>, <span class="font-mono">host</span>,
              <span class="font-mono">port</span>, <span class="font-mono">password</span> and
              <span class="font-mono">tls</span>. A connection to a server the platform does not run cannot be
              asked for any of this and refuses the claim saying so, rather than binding a server that will not
              behave the way the claim assumed.
            </p>
          </template>

          <template v-if="isObjectStore">
            <div class="grid gap-4 sm:grid-cols-2">
              <UFormField
                label="Versioning"
                help="Keep every version of an object, so an overwrite or a delete can be undone at the store."
              >
                <USwitch v-model="bucketVersioning" />
              </UFormField>
              <UFormField
                label="Public read"
                help="Anyone can read the bucket's objects without a credential. Only a store on the internet can honour it; the bundled store refuses it, saying so."
              >
                <USwitch v-model="bucketPublicRead" />
              </UFormField>
            </div>
            <UFormField label="Size" help="A Kubernetes quantity the bucket may not grow past. Empty asks for no limit.">
              <UInput v-model="bucketSize" placeholder="50Gi" class="w-full font-mono" />
            </UFormField>
            <p class="text-xs text-muted">
              The secret carries <span class="font-mono">endpoint</span>, <span class="font-mono">bucket</span>,
              <span class="font-mono">region</span>, <span class="font-mono">accessKeyId</span>,
              <span class="font-mono">secretAccessKey</span> and <span class="font-mono">forcePathStyle</span> — a
              credential scoped to this one bucket, never the store's own. A requirement the store cannot honour
              fails the claim with the reason rather than provisioning something else.
            </p>
          </template>

          <div v-if="isPostgres" class="grid gap-4 sm:grid-cols-2">
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
          <p v-if="isPostgres" class="text-xs text-muted">
            A version or an extension the connection cannot supply fails the claim with a message saying what is
            available — rather than binding and letting the application die on a
            <span class="font-mono">CREATE EXTENSION</span> later. A connection to a hosted Postgres cannot be asked
            for either, and says so.
          </p>

          <div v-if="isPostgres" class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Storage" help="A Kubernetes quantity. Empty takes the platform's default.">
              <UInput v-model="pgStorageSize" placeholder="10Gi" class="w-full font-mono" />
            </UFormField>
            <UFormField label="Storage class" help="Empty takes the platform's default.">
              <UInput v-model="pgStorageClass" placeholder="fast-ssd" class="w-full font-mono" />
            </UFormField>
          </div>
          <p v-if="isPostgres" class="text-xs text-muted">
            All four are applied when the database is created. Changing them afterwards asks for a different
            database rather than reshaping this one.
          </p>

          <UFormField
            v-if="isPostgres"
            label="On claim deletion"
            :help="`Retain is the default: deleting a claim must not be able to destroy a production ${resourceNoun}. Preview resources are always cleaned up.`"
          >
            <USelect v-model="deletionPolicy" :items="policyOptions" class="w-full" />
            <p v-if="destroyRefusal" class="mt-1 text-xs text-muted">{{ destroyRefusal }}.</p>
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
