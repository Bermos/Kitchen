<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api } from "../lib/api";
import { connectionChoices, noteFor, selectableChoices, type ConnectionChoice } from "../lib/connections";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";

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
