<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api } from "../lib/api";

// The create flow for resource claims: ask a database-capable connection to
// provision something for this project. The binding credentials stay in the
// cluster — the reconciler writes them into a secret the project's env vars
// reference; nothing here ever sees them.

const props = defineProps<{ project: string }>();
const emit = defineEmits<{ saved: [] }>();

const toast = useToast();
const open = ref(false);

const name = ref("");
const connection = ref("");
const previewBranching = ref(false);
const deletionPolicy = ref("Retain");

// The one claim type the platform provisions today. The field exists so the
// day a second type lands, this is a select instead of a redesign.
const type = ref("postgres");
const typeOptions = [{ label: "postgres", value: "postgres" }];

const policyOptions = [
  { label: "Retain — keep the database when the claim is deleted", value: "Retain" },
  { label: "Delete — destroy the database and its data with the claim", value: "Delete" },
];

const connections = ref<{ label: string; value: string }[]>([]);
const connectionsLoaded = ref(false);

// Only connections that can actually provision a database are offered. A
// connection that has not reported capabilities yet (the operator has not
// validated it) is offered with a caveat rather than hidden — the reconciler
// holds the claim Pending until validation either way.
async function loadConnections() {
  connectionsLoaded.value = false;
  try {
    const all = await api.connections();
    connections.value = all
      .filter((c) => !c.capabilities?.length || c.capabilities.includes("database"))
      .map((c) => ({
        label: c.capabilities?.length ? `${c.name} (${c.provider})` : `${c.name} (${c.provider}, not validated yet)`,
        value: c.name,
      }));
  } catch {
    connections.value = [];
  } finally {
    connectionsLoaded.value = true;
  }
}

watch(open, (value) => {
  if (!value) return;
  name.value = "";
  connection.value = "";
  previewBranching.value = false;
  deletionPolicy.value = "Retain";
  void loadConnections();
});

const ready = computed(() => Boolean(name.value && connection.value));

const saving = ref(false);
async function save() {
  if (!ready.value || saving.value) return;
  saving.value = true;
  try {
    const created = await api.createClaim({
      name: name.value,
      project: props.project,
      connection: connection.value,
      type: type.value,
      previewBranching: previewBranching.value,
      deletionPolicy: deletionPolicy.value,
    });
    toast.add({ title: `Claim ${created.name} created`, color: "success", icon: "i-lucide-database" });
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
    :description="`Ask a connection to provision a database for ${props.project}. The credentials land in a secret the project's env vars can reference — they are never shown here.`"
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

        <UFormField label="Connection" help="A connection with the database capability provisions and owns the instance." required>
          <USelect
            v-model="connection"
            :items="connections"
            :placeholder="connectionsLoaded && !connections.length ? 'No database-capable connections' : 'Select a connection'"
            :disabled="connectionsLoaded && !connections.length"
            class="w-full"
          />
        </UFormField>
        <p v-if="connectionsLoaded && !connections.length" class="text-xs text-muted">
          No connection can provision databases — create one first (e.g. a Neon connection) on the Connections page.
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
