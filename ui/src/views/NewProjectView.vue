<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRouter } from "vue-router";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";
import { api, CRITICALITIES, DATA_CLASSES, type Detection } from "../lib/api";
import {
  connectionChoices,
  defaultBranchFor,
  noteFor,
  repositoryChoices,
  repositoryNote,
  repositoryURLFor,
  selectableChoices,
} from "../lib/connections";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { useAsync } from "../lib/useAsync";

// Creating a project.
//
// This was a modal, and it stopped fitting one twice over. A dialog is a
// viewport-height box with a scrolling body, so on a phone the create flow was
// a form read through a letterbox with its own Create button below the fold —
// and the thing being created is a project, which is the largest object this
// platform has. A screen is the honest shape for it: the address is a link
// somebody can send, the browser's back button means what it says, and the
// form is as long as the questions are.
//
// It is also what made the compliance declarations possible. In a dialog they
// would have been a fifth and sixth field on a form already too long for the
// box, so a project's class and its designation were left to their defaults
// and corrected — or not — from a settings pane afterwards. A default nobody
// chose is exactly what a classification must never be, so they are asked
// here, they are not pre-selected, and the project cannot be created until
// somebody has answered them. "Unclassified" is one of the answers; it is an
// answer and not a silence.
//
// Creating a project is self-service: any account may, and becomes its admin.
// So the two connection fields are picked from what `GET /connections` answers
// a member with — names, capabilities and readiness — and nothing here offers
// to create, test or change one. That is the operator's screen, and a member
// is not sent to a page they cannot open.

const router = useRouter();
const toast = useToast();

const connections = useAsync(() => api.connections());

// Which kind of project is being created. A project's source is a repository
// this platform builds or an image somebody else built, never both (#307), so
// this is one form with two shapes rather than two flows: the name, the
// preview switch and the create button are the same question either way, and
// only what is between them changes.
const sourceKind = ref<"repo" | "image">("repo");
const sourceKinds = [
  { label: "A repository", value: "repo" },
  { label: "An image", value: "image" },
];
const fromRepo = computed(() => sourceKind.value === "repo");

// The vendored image: where it lives, which version of it to run, and what it
// is pulled with. The pull credential is optional and left out by default,
// because the great majority of vendored images are public — asking for a
// Connection would be one somebody had to invent.
const imageRepository = ref("");
const imageVersion = ref("");
const imageConnection = ref<string>();

// A digest is `sha256:` and sixty-four hex digits; anything else is a tag.
// One field rather than two, because a vendor publishes one string and
// nobody thinks of it as two kinds of thing.
const versionIsDigest = computed(() => /^sha256:[a-f0-9]{64}$/.test(imageVersion.value.trim()));

const name = ref("");
const nameEdited = ref(false);
const repo = ref("");
const connection = ref<string>();
const registry = ref<string>();
const productionBranch = ref("main");
const branchEdited = ref(false);
const previews = ref(true);
// Whether the project is on the internet. It is asked here rather than left to
// the settings screen because a project that exists to be called by other
// applications should never have been published at all — not even for the
// minute between creating it and remembering to change it.
const internal = ref(false);

// Every connection is listed, and the ones that cannot back this field say
// why: a connection providing the wrong capability is refused by the API
// (`requireConnection`) so it is disabled, and one the platform has not got
// working — or has not assessed at all — is offered with the caveat, because
// the API takes it and the project's own conditions are what say whether it
// worked. Omitting either would leave somebody hunting for a connection they
// have been told exists.
const sourceOptions = computed(() => connectionChoices(connections.data.value ?? [], "gitSource"));
const registryOptions = computed(() => connectionChoices(connections.data.value ?? [], "imageStore"));
const sourcesAvailable = computed(() => selectableChoices(sourceOptions.value));
const registriesAvailable = computed(() => selectableChoices(registryOptions.value));
const sourceNote = computed(() => noteFor(sourceOptions.value, connection.value));
const registryNote = computed(() => noteFor(registryOptions.value, registry.value));

// Only an operator can do anything about a missing connection, so only an
// operator is pointed at the screen where it is done.
const managesConnections = computed(() => may("POST /api/v1/connections", callerFor()));

// A field with exactly one answer is not a question. The platform seeds a
// registry connection pointing at the one it runs itself, so on a fresh
// installation this is the whole of choosing where images go — and the same
// reasoning covers the single git connection most people have. Anything
// already picked by hand is left alone.
//
// It is deliberately not what the compliance section does. A single obvious
// answer is a question not worth asking; a classification has no obvious
// answer, and filling one in would be the platform deciding.
watch(registriesAvailable, (options) => {
  if (!registry.value && options.length === 1) registry.value = options[0]!.value;
});
watch(sourcesAvailable, (options) => {
  if (!connection.value && options.length === 1) connection.value = options[0]!.value;
});

// The repository field, for the providers the platform can ask. A git
// connection holds a credential that already knows which repositories it can
// see, so the field is a list to filter rather than a name to spell — and the
// listing is fetched per connection, because it is that connection's
// credential that answers it.
//
// Nothing here can take the typed field away: a provider with no
// implementation, a token the provider refused, and a listing cut short at
// the cap all end with somebody who still has a repository to name. So the
// select is offered when there is something to offer, `createItem` accepts a
// name that is not in the list, and the plain input is what is left otherwise
// — with `repositoryNote` saying which of those happened.
const repositories = useAsync(() => api.connectionRepositories(connection.value!), { immediate: false });
watch(connection, (value) => {
  if (value) void repositories.refresh();
});

// A repository typed into the select rather than chosen from it. Kept as an
// entry of its own so the field shows what it holds, the way a chosen one is.
const typedRepo = ref("");
// A failed listing offers nothing rather than the previous connection's
// repositories, which is what the field would otherwise be left holding.
const listedRepos = computed(() =>
  repositories.error.value ? [] : repositoryChoices(repositories.data.value ?? undefined),
);
const repoOptions = computed(() => {
  const listed = listedRepos.value;
  if (!typedRepo.value || listed.some((choice) => choice.value === typedRepo.value)) return listed;
  return [{ label: typedRepo.value, value: typedRepo.value, description: "typed in" }, ...listed];
});
// The select is only worth drawing when it has something to list; everything
// else is the input with a line under it saying why.
const canPickRepo = computed(() => listedRepos.value.length > 0);
// Where the chosen repository is on the provider's own site, so it can be
// looked at before it is connected. It comes from the listing, which is the
// API's answer — nothing here composes a URL (#435).
const repoURL = computed(() => repositoryURLFor(repositories.data.value ?? undefined, repo.value));
const repoNote = computed(() =>
  repositoryNote(repositories.data.value ?? undefined, repositories.error.value ?? undefined),
);

function repoTypedIn(term: string) {
  typedRepo.value = term;
  repo.value = term;
}

// The preflight: read the repository the way a build would and say what the
// platform makes of it, while the build context is still a form field. It is
// the difference between "the root directory is one level off" and a build
// that fails five minutes after the project was created and reads like the
// platform is broken.
//
// It is asked for whenever the three things it depends on settle, and it
// writes nothing, so asking again is the whole of correcting a wrong answer.
const rootDirectory = ref("");
const dockerfilePath = ref("");
const dockerfileTarget = ref("");
const detection = ref<Detection>();
const detecting = ref(false);
const detectError = ref("");
// Only the newest answer is shown: a slow reply to an older build context
// would otherwise overwrite the one somebody is looking at.
let detectRun = 0;

async function detect() {
  const run = ++detectRun;
  detection.value = undefined;
  detectError.value = "";
  if (!connection.value || !repo.value.includes("/")) return;
  detecting.value = true;
  try {
    const answer = await api.detectRepository(connection.value, {
      repo: repo.value,
      ref: productionBranch.value || undefined,
      rootDirectory: rootDirectory.value || undefined,
      dockerfilePath: dockerfilePath.value || undefined,
    });
    if (run === detectRun) detection.value = answer;
  } catch (err) {
    // A preflight that cannot run is not a reason to block the form: the
    // build is still what decides, and it is allowed to disagree with a
    // provider that would not answer a question a minute ago.
    if (run === detectRun) detectError.value = err instanceof Error ? err.message : String(err);
  } finally {
    if (run === detectRun) detecting.value = false;
  }
}

// The stages the preflight found, as something to choose from: the file's
// last stage first, because that is what a build ships when nothing says
// otherwise. A stage the answer no longer offers is dropped rather than kept
// as a value nothing in the list matches — the repository the form is now
// about is a different one.
const stageOptions = computed(() => [
  { label: "the last stage", value: "" },
  ...(detection.value?.stages ?? []).map((stage) => ({ label: stage, value: stage })),
]);
watch(stageOptions, (options) => {
  if (!options.some((option) => option.value === dockerfileTarget.value)) dockerfileTarget.value = "";
});

// Typing settles before the provider is asked, and every field the answer is
// about restarts the clock.
let detectTimer: ReturnType<typeof setTimeout> | undefined;
watch([repo, connection, productionBranch, rootDirectory, dockerfilePath], () => {
  clearTimeout(detectTimer);
  detectTimer = setTimeout(() => void detect(), 400);
});

// What the verdict reads as. A framework nobody recognised is a warning
// rather than an error, because creating the project anyway is a legitimate
// choice — the build strategy can be set afterwards. A repository that could
// not be read is not that: nothing was looked at, the fields below it are not
// what is wrong, and no build of it can succeed either.
const detectionColor = computed(() => {
  if (detection.value?.unreadable) return "error";
  return detection.value?.detected ? "success" : "warning";
});
const detectionIcon = computed(() =>
  detection.value?.detected ? "i-lucide-check" : "i-lucide-triangle-alert",
);
const detectionTitle = computed(() => {
  const found = detection.value;
  if (!found) return "";
  if (found.unreadable) return "The repository could not be read";
  if (!found.detected) return "No framework detected";
  const how = found.dockerfile ? "Dockerfile" : `built with ${found.strategy}`;
  return `Detected ${found.framework} — ${how}`;
});

// A chosen repository knows which branch it deploys from, so the production
// branch follows it — until it is edited by hand, at which point it is the
// user's, exactly as the name is.
watch(repo, (value) => {
  const branch = defaultBranchFor(repositories.data.value ?? undefined, value);
  if (branch && !branchEdited.value) productionBranch.value = branch;
});

// "acme/shop" suggests the name "shop", cut down to what the API accepts —
// until the name is edited by hand, at which point it is the user's. An image
// reference suggests one the same way, from its last path segment.
function suggestName(from: string) {
  if (nameEdited.value) return;
  const tail = from.split("/").pop() ?? "";
  name.value = tail
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 46);
}
watch(repo, suggestName);
watch(imageRepository, suggestName);

// ── What the institution declares ───────────────────────────────────────────

// The two declarations, and the sentinel each of them uses for "none".
//
// They are `undefined` until somebody answers, and "none" is a value in the
// list rather than the absence of one, so that declining to classify is a
// thing a person did and not a field they scrolled past. The API takes an
// empty string for both, meaning unclassified and undesignated, and reads an
// *absent* field as "the caller said nothing" — which is why the sentinels
// are translated at the point of sending rather than stored as "".
const UNCLASSIFIED = "unclassified";
const UNDESIGNATED = "undesignated";

const dataClass = ref<string>();
const criticality = ref<string>();
const rto = ref("");
const rpo = ref("");

const dataClassOptions = [
  { label: "Unclassified — no class of the four applies", value: UNCLASSIFIED },
  ...DATA_CLASSES.map((value) => ({ label: value as string, value: value as string })),
];
const criticalityOptions = [
  { label: "Undesignated — nobody has designated this function", value: UNDESIGNATED },
  ...CRITICALITIES.map((value) => ({ label: value as string, value: value as string })),
];

/** Whether a designation was given, which is what makes a tolerance mean
 *  anything: how long a function may be down is a sentence about a function
 *  somebody has designated. */
const designated = computed(() => Boolean(criticality.value) && criticality.value !== UNDESIGNATED);

// The CRD's own rule, spelled here so that a mistyped tolerance is caught
// while the form is open rather than by a round trip that loses the rest of
// it. `4h`, `30m`, `1h30m`; empty is no tolerance recorded, which is allowed
// and said in words under the field.
const TOLERANCE = /^([0-9]+h)?([0-9]+m)?$/;
function toleranceError(value: string): string {
  const written = value.trim();
  if (!written) return "";
  return TOLERANCE.test(written) ? "" : "Whole hours and minutes — 4h, 30m, 1h30m.";
}
const rtoError = computed(() => toleranceError(rto.value));
const rpoError = computed(() => toleranceError(rpo.value));

/** What the answers say, or the sentence saying nothing was said. It is under
 *  the section rather than only in the fields, because the thing worth reading
 *  back before creating a project is the declaration as a whole. */
const declaration = computed(() => {
  if (!dataClass.value || !criticality.value) return "";
  const parts = [
    dataClass.value === UNCLASSIFIED ? "unclassified" : dataClass.value,
    criticality.value === UNDESIGNATED ? "undesignated" : criticality.value,
  ];
  if (designated.value && rto.value.trim()) parts.push(`RTO ${rto.value.trim()}`);
  if (designated.value && rpo.value.trim()) parts.push(`RPO ${rpo.value.trim()}`);
  return parts.join(" · ");
});

// ── Creating it ─────────────────────────────────────────────────────────────

/** What is still missing, in the order the form asks for it. The button says
 *  it is disabled; this says what would un-disable it, which on a form this
 *  long is the difference between a decision and a hunt. */
const missing = computed(() => {
  const wanted: string[] = [];
  if (fromRepo.value) {
    if (!repo.value.includes("/")) wanted.push("a repository");
    if (!connection.value) wanted.push("a git connection");
    if (!registry.value) wanted.push("a registry");
  } else {
    if (!imageRepository.value.trim()) wanted.push("an image repository");
    if (!imageVersion.value.trim()) wanted.push("a version");
  }
  if (!name.value) wanted.push("a name");
  if (!dataClass.value) wanted.push("a data classification");
  if (!criticality.value) wanted.push("a criticality");
  if (rtoError.value || rpoError.value) wanted.push("a tolerance the platform can read");
  return wanted;
});
const ready = computed(() => missing.value.length === 0);

const creating = ref(false);
async function create() {
  if (!ready.value || creating.value) return;
  creating.value = true;
  try {
    // The declaration, in the API's vocabulary: the sentinels become the
    // empty string, which is what "unclassified" and "undesignated" are
    // written as. They are sent either way — an empty string is the answer
    // somebody gave, and it is recorded as one.
    const declared = {
      dataClass: dataClass.value === UNCLASSIFIED ? "" : dataClass.value,
      criticality: criticality.value === UNDESIGNATED ? "" : criticality.value,
      rto: designated.value ? rto.value.trim() : "",
      rpo: designated.value ? rpo.value.trim() : "",
    };
    // Only the fields the chosen source has. A repository's settings sent
    // with an image are refused by the API rather than ignored, which is the
    // right answer and a poor thing to make somebody read.
    const version = imageVersion.value.trim();
    const project = await api.createProject(
      fromRepo.value
        ? {
            name: name.value,
            repo: repo.value,
            connection: connection.value!,
            registry: registry.value!,
            productionBranch: productionBranch.value || undefined,
            previews: previews.value,
            exposure: internal.value ? "internal" : undefined,
            rootDirectory: rootDirectory.value || undefined,
            dockerfilePath: dockerfilePath.value || undefined,
            dockerfileTarget: dockerfileTarget.value || undefined,
            ...declared,
          }
        : {
            name: name.value,
            exposure: internal.value ? "internal" : undefined,
            image: {
              repository: imageRepository.value.trim(),
              tag: versionIsDigest.value ? undefined : version,
              digest: versionIsDigest.value ? version : undefined,
              connection: imageConnection.value || undefined,
            },
            ...declared,
          },
    );
    toast.add({ title: `Project ${project.name} created`, color: "success", icon: "i-lucide-check" });
    void router.push({ name: "project", params: { name: project.name } });
  } catch (err) {
    toast.add({
      title: "Creating the project failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    creating.value = false;
  }
}
</script>

<template>
  <div class="space-y-6 max-w-3xl">
    <PageHeader title="New project" :breadcrumb="[{ label: 'Projects', to: '/projects' }, { label: 'New project' }]">
      <template #description>
        What this project is built from, what it is called, who may reach it, and what the institution declares
        about the data it handles.
      </template>
    </PageHeader>

    <UAlert
      v-if="connections.error.value"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="connections.error.value"
    />

    <form class="space-y-6" @submit.prevent="create">
      <PageSection title="Source" description="Where this project's software comes from — one thing or the other.">
        <div class="space-y-4">
          <!-- Which kind of project. It is first because everything under it
               depends on the answer, and it is two buttons rather than two
               screens because the name, the previews and the create button are
               the same question either way. -->
          <UTabs v-model="sourceKind" :items="sourceKinds" :content="false" size="sm" class="w-full" />

          <template v-if="!fromRepo">
            <UFormField
              label="Image repository"
              help="Where the image lives, registry host included and without a tag — ghcr.io/home-assistant/home-assistant."
              required
            >
              <UInput v-model="imageRepository" placeholder="ghcr.io/acme/thing" class="w-full font-mono" autofocus />
            </UFormField>
            <UFormField
              label="Version"
              :help="
                versionIsDigest
                  ? 'A digest: the exact content, which never moves.'
                  : 'A tag as the vendor publishes it, or a sha256: digest to pin the exact content.'
              "
              required
            >
              <UInput v-model="imageVersion" placeholder="2026.9.1" class="w-full font-mono" />
            </UFormField>
            <UFormField
              label="Pull credential"
              :help="
                imageConnection
                  ? registryNote
                  : 'Leave it empty for a public image, which is pulled anonymously. This is never the connection a project pushes its own builds to.'
              "
            >
              <USelect
                v-model="imageConnection"
                :items="registryOptions"
                :loading="connections.loading.value"
                placeholder="None — a public image"
                class="w-full"
              />
            </UFormField>
            <p class="text-xs text-muted">
              There is no repository, so there is nothing to build, nowhere to push and no pull request to preview.
              The digest this resolves to is what the release records, so a rollback restores the exact image it ran.
            </p>
          </template>

          <template v-else>
            <UFormField label="Repository" :help="repoNote" required>
              <USelectMenu
                v-if="canPickRepo"
                v-model="repo"
                :items="repoOptions"
                value-key="value"
                :loading="repositories.loading.value"
                :create-item="true"
                placeholder="acme/shop"
                class="w-full font-mono"
                autofocus
                @create="repoTypedIn"
              />
              <UInput
                v-else
                v-model="repo"
                placeholder="acme/shop"
                class="w-full font-mono"
                :loading="repositories.loading.value"
                autofocus
              />
              <template v-if="repoURL" #hint>
                <a :href="repoURL" target="_blank" rel="noopener" class="text-xs text-primary hover:underline">open</a>
              </template>
            </UFormField>
            <div class="grid gap-4 sm:grid-cols-2">
              <UFormField label="Git connection" :help="sourceNote" required>
                <USelect
                  v-model="connection"
                  :items="sourceOptions"
                  :loading="connections.loading.value"
                  placeholder="Select…"
                  class="w-full"
                />
              </UFormField>
              <UFormField label="Registry" :help="registryNote" required>
                <USelect
                  v-model="registry"
                  :items="registryOptions"
                  :loading="connections.loading.value"
                  placeholder="Select…"
                  class="w-full"
                />
              </UFormField>
            </div>
            <p
              v-if="connections.data.value && (!sourcesAvailable.length || !registriesAvailable.length)"
              class="text-xs text-warning"
            >
              {{ !sourcesAvailable.length ? "No gitSource connection yet" : "No imageStore connection yet" }} —
              <template v-if="managesConnections">
                create one on the <RouterLink to="/platform/connections" class="underline">Connections</RouterLink>
                page first.
              </template>
              <template v-else>ask an operator to add one before creating this project.</template>
            </p>
            <UFormField label="Production branch" help="Builds of this branch promote to production.">
              <UInput v-model="productionBranch" class="w-full sm:w-56 font-mono" @input="branchEdited = true" />
            </UFormField>
          </template>
        </div>
      </PageSection>

      <!-- What the platform makes of the repository, and the two fields that
           change the answer. Both are optional and both are what a monorepo
           or an unconventionally-placed Dockerfile needs, so they sit with
           the verdict they explain rather than in a settings page reached
           after the first build has already failed. -->
      <PageSection
        v-if="fromRepo"
        title="Build"
        description="What is built, and what the platform makes of it. Creating the project starts a build of the production branch, so this is asked now rather than after one has failed."
      >
        <div class="space-y-4">
          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Root directory" help="The directory that is built. Empty is the repository itself.">
              <UInput v-model="rootDirectory" placeholder="apps/shop" class="w-full font-mono" />
            </UFormField>
            <UFormField label="Dockerfile path" help="Relative to the root directory. Empty is ./Dockerfile.">
              <UInput v-model="dockerfilePath" placeholder="Dockerfile" class="w-full font-mono" />
            </UFormField>
          </div>
          <!-- Only for a Dockerfile that has stages to choose between. A
               single-stage file names none, and asking which stage to ship
               where there is one would be a question about every repository.
               It is offered here rather than only in settings because the
               project's first build starts as it is created, and a build of
               the wrong stage succeeds. -->
          <UFormField
            v-if="stageOptions.length > 1"
            label="Stage to ship"
            help="Which stage of the multi-stage Dockerfile produces the image to run."
          >
            <USelect v-model="dockerfileTarget" :items="stageOptions" class="w-full sm:max-w-60" />
          </UFormField>
          <p v-if="detecting" class="text-xs text-muted flex items-center gap-1.5">
            <UIcon name="i-lucide-loader-circle" class="animate-spin" />
            Reading the repository…
          </p>
          <UAlert
            v-else-if="detection"
            :color="detectionColor"
            variant="soft"
            :icon="detectionIcon"
            :title="detectionTitle"
            :description="
              detection.detected
                ? `${detection.rootDirectory || '.'} at ${detection.ref}${detection.port ? ` — listens on ${detection.port}` : ''}`
                : detection.message
            "
          />
          <p v-else-if="detectError" class="text-xs text-muted">
            The layout could not be checked ({{ detectError }}) — the build decides.
          </p>
          <p v-if="detection?.files?.length" class="text-xs text-muted font-mono break-all">
            {{ detection.files.join("  ") }}
          </p>
        </div>
      </PageSection>

      <PageSection title="Name and reach" description="What this project is called, and who can get to it.">
        <div class="space-y-4">
          <UFormField
            label="Name"
            help="Lowercase letters, digits and dashes — it names URLs, builds and every address generated for this project."
            required
          >
            <UInput v-model="name" class="w-full font-mono" @input="nameEdited = true" />
          </UFormField>
          <USwitch
            v-if="fromRepo"
            v-model="previews"
            label="Preview environments"
            description="Every pull request gets its own environment, gated behind platform login."
          />
          <USwitch
            v-model="internal"
            label="Internal — not on the internet"
            description="For a project the other applications here call rather than people visit. No environment of it
              is published: no hostname, no certificate and no preview gate, previews included. Each one still runs and
              is reachable from the other applications on this platform, and none of them can idle. It can be changed
              afterwards."
          />
        </div>
      </PageSection>

      <!-- The declarations, and the one section on this form with nothing
           filled in.

           The copy leads with what Kitchen does not do, because a form that
           asked "how critical is this?" without saying whose question it is
           would read as the platform having an opinion about the institution's
           functions. It does not — and a platform that quietly answered for
           somebody would be worse than one that never asked. -->
      <PageSection
        title="Classification and continuity"
        description="What class of data this project will handle, and how much its function matters. Both are required before the project is created, and neither is filled in for you."
      >
        <div class="space-y-4">
          <p class="text-xs text-muted">
            <span class="text-toned font-medium">
              Kitchen does not classify anything, does not decide what is critical, and does not set these
              tolerances.
            </span>
            They are the institution's — a judgement about its own data and its own functions — and this is where
            that judgement is recorded, at the moment the project is created rather than in a settings pane
            afterwards. Nothing here refuses a deployment. What the platform does with the answers is narrow what a
            claim may hold, refuse a promotion into an environment rated below the class, map the function onto
            everything serving it, and alert against the tolerance. Every answer is audit-logged.
          </p>

          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField
              label="Data classification"
              help="The sensitivity of what this project handles. Attached resources narrow it and never exceed it."
              required
            >
              <USelect v-model="dataClass" :items="dataClassOptions" placeholder="Choose…" class="w-full" />
            </UFormField>
            <UFormField
              label="Criticality"
              help="How much it matters that this function keeps working. Production environments read this where they declare nothing of their own; previews never do."
              required
            >
              <USelect v-model="criticality" :items="criticalityOptions" placeholder="Choose…" class="w-full" />
            </UFormField>
          </div>

          <!-- The tolerances are a sentence about a designated function, so
               they are asked once there is one. Undesignated says so instead
               of showing two fields nothing would read. -->
          <div v-if="designated" class="rounded-md border border-default bg-muted p-4 space-y-4">
            <h3 class="text-xs font-medium text-highlighted">Disruption tolerances</h3>
            <div class="grid gap-4 sm:grid-cols-2">
              <UFormField
                label="RTO"
                help="How long it may be unavailable. Whole hours and minutes: 4h, 30m, 1h30m. It is the threshold the outage alert fires against."
                :error="rtoError || undefined"
              >
                <UInput v-model="rto" placeholder="4h" class="w-full font-mono" />
              </UFormField>
              <UFormField
                label="RPO"
                help="How much data it may lose. Same spelling. Carried and mapped; nothing alerts on it yet, because the platform observes no recovery points."
                :error="rpoError || undefined"
              >
                <UInput v-model="rpo" placeholder="30m" class="w-full font-mono" />
              </UFormField>
            </div>
            <p class="text-xs text-muted">
              Either can be left empty, and then no tolerance is recorded — which is not the same as zero, and is
              what the compliance screens will report.
            </p>
          </div>
          <p v-else-if="criticality" class="text-xs text-muted">
            An undesignated function has no tolerances to set: how long something may be down is a statement about a
            function somebody has designated. Both can be declared later, on the project's Continuity settings.
          </p>

          <p v-if="declaration" class="text-xs text-toned font-mono">{{ declaration }}</p>
        </div>
      </PageSection>

      <div class="flex flex-col-reverse sm:flex-row sm:items-center sm:justify-end gap-3 pt-2 border-t border-default">
        <p v-if="missing.length" class="text-xs text-muted sm:mr-auto">
          Still needed: {{ missing.join(", ") }}.
        </p>
        <div class="flex items-center justify-end gap-2">
          <UButton color="neutral" variant="ghost" to="/">Cancel</UButton>
          <UButton type="submit" :disabled="!ready" :loading="creating" icon="i-lucide-plus">Create project</UButton>
        </div>
      </div>
    </form>
  </div>
</template>
