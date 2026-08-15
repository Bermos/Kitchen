<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRouter } from "vue-router";
import { api } from "../lib/api";
import { useAsync } from "../lib/useAsync";

// The create flow the empty states used to point at kubectl for: a Project is
// a name, a repository, and the two Connections it builds and stores images
// with — everything else keeps its default. The default slot is the trigger,
// so each spot that opens this chooses its own button.

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
const previews = ref(true);

// A Connection that has not reported capabilities yet (the operator has not
// assessed it) stays selectable — the API accepts it and the project's own
// conditions say whether it fits.
function withCapability(capability: string) {
  return (connections.data.value ?? [])
    .filter((c) => !c.capabilities?.length || c.capabilities.includes(capability))
    .map((c) => ({ label: `${c.name} · ${c.provider}`, value: c.name }));
}
const sourceOptions = computed(() => withCapability("gitSource"));
const registryOptions = computed(() => withCapability("imageStore"));

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
        <UFormField label="Repository" help="owner/name on the provider the git connection points at." required>
          <UInput v-model="repo" placeholder="acme/shop" class="w-full font-mono" autofocus />
        </UFormField>
        <UFormField
          label="Name"
          help="Lowercase letters, digits and dashes — it names URLs, builds and namespaces."
          required
        >
          <UInput v-model="name" class="w-full font-mono" @input="nameEdited = true" />
        </UFormField>
        <div class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Git connection" required>
            <USelect
              v-model="connection"
              :items="sourceOptions"
              :loading="connections.loading.value"
              placeholder="Select…"
              class="w-full"
            />
          </UFormField>
          <UFormField label="Registry" required>
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
          v-if="connections.data.value && (!sourceOptions.length || !registryOptions.length)"
          class="text-xs text-warning"
        >
          {{ !sourceOptions.length ? "No gitSource connection yet" : "No imageStore connection yet" }} — create one
          on the <RouterLink to="/connections" class="underline">Connections</RouterLink> page first.
        </p>
        <UFormField label="Production branch" help="Builds of this branch promote to production.">
          <UInput v-model="productionBranch" class="w-44 font-mono" />
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
