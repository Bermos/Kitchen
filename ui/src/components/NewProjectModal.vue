<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRouter } from "vue-router";
import { api } from "../lib/api";
import {
  connectionChoices,
  defaultBranchFor,
  noteFor,
  repositoryChoices,
  repositoryNote,
  selectableChoices,
} from "../lib/connections";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { useAsync } from "../lib/useAsync";

// The create flow the empty states used to point at kubectl for: a Project is
// a name, a repository, and the two Connections it builds and stores images
// with — everything else keeps its default. The default slot is the trigger,
// so each spot that opens this chooses its own button.
//
// Creating a project is self-service: any account may, and becomes its admin.
// So the two connection fields are picked from what `GET /connections` answers
// a member with — names, capabilities and readiness — and nothing here offers
// to create, test or change one. That is the operator's screen, and a member
// is not sent to a page they cannot open.

const emit = defineEmits<{ created: [] }>();

const router = useRouter();
const toast = useToast();
const open = ref(false);

// Connections are fetched when the modal opens, so the selects are as fresh
// as the moment of use rather than the page load.
const connections = useAsync(() => api.connections(), { immediate: false });
watch(open, (value) => {
  if (value) void connections.refresh();
});

const name = ref("");
const nameEdited = ref(false);
const repo = ref("");
const connection = ref<string>();
const registry = ref<string>();
const productionBranch = ref("main");
const branchEdited = ref(false);
const previews = ref(true);

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
const repoNote = computed(() =>
  repositoryNote(repositories.data.value ?? undefined, repositories.error.value ?? undefined),
);

function repoTypedIn(term: string) {
  typedRepo.value = term;
  repo.value = term;
}

// A chosen repository knows which branch it deploys from, so the production
// branch follows it — until it is edited by hand, at which point it is the
// user's, exactly as the name is.
watch(repo, (value) => {
  const branch = defaultBranchFor(repositories.data.value ?? undefined, value);
  if (branch && !branchEdited.value) productionBranch.value = branch;
});

// "acme/shop" suggests the name "shop", cut down to what the API accepts —
// until the name is edited by hand, at which point it is the user's.
watch(repo, (value) => {
  if (nameEdited.value) return;
  const tail = value.split("/").pop() ?? "";
  name.value = tail
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 46);
});

const ready = computed(() =>
  Boolean(name.value && repo.value.includes("/") && connection.value && registry.value),
);

const creating = ref(false);
async function create() {
  if (!ready.value || creating.value) return;
  creating.value = true;
  try {
    const project = await api.createProject({
      name: name.value,
      repo: repo.value,
      connection: connection.value!,
      registry: registry.value!,
      productionBranch: productionBranch.value || undefined,
      previews: previews.value,
    });
    open.value = false;
    name.value = "";
    nameEdited.value = false;
    repo.value = "";
    typedRepo.value = "";
    branchEdited.value = false;
    toast.add({ title: `Project ${project.name} created`, color: "success", icon: "i-lucide-check" });
    emit("created");
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
  <UModal
    v-model:open="open"
    title="New project"
    description="Connect a repository and Kitchen builds and deploys it."
  >
    <slot>
      <UButton icon="i-lucide-plus" size="sm">New project</UButton>
    </slot>

    <template #body>
      <form class="space-y-4" @submit.prevent="create">
        <UAlert
          v-if="connections.error.value"
          color="error"
          variant="soft"
          icon="i-lucide-triangle-alert"
          :title="connections.error.value"
        />
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
        </UFormField>
        <UFormField
          label="Name"
          help="Lowercase letters, digits and dashes — it names URLs, builds and namespaces."
          required
        >
          <UInput v-model="name" class="w-full font-mono" @input="nameEdited = true" />
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
            create one on the <RouterLink to="/connections" class="underline">Connections</RouterLink> page first.
          </template>
          <template v-else>ask an operator to add one before creating this project.</template>
        </p>
        <UFormField label="Production branch" help="Builds of this branch promote to production.">
          <UInput v-model="productionBranch" class="w-44 font-mono" @input="branchEdited = true" />
        </UFormField>
        <USwitch
          v-model="previews"
          label="Preview environments"
          description="Every pull request gets its own environment, gated behind platform login."
        />
      </form>
    </template>

    <template #footer>
      <div class="flex justify-end gap-2 w-full">
        <UButton color="neutral" variant="ghost" @click="open = false">Cancel</UButton>
        <UButton :disabled="!ready" :loading="creating" icon="i-lucide-plus" @click="create">
          Create project
        </UButton>
      </div>
    </template>
  </UModal>
</template>
