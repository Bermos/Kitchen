<script setup lang="ts">
import { computed, ref } from "vue";
import { api } from "../lib/api";
import { isOperator } from "../lib/me";

// Declaring an environment before anything deploys into it (#491).
//
// An environment used to appear only when a build put a release in it, so the
// bar its owners set could only ever be raised after the first release had
// already landed. This is the other order: the environment first, its bar
// next, and the first build judged against it.
//
// **One field, deliberately.** The type is derived from the project's
// promotion pipeline and refused by the API when it disagrees, so there is
// nothing to ask; and the bar, the classification and the tolerances are the
// environment owners' declaration, which an environment that does not exist
// yet has nobody for. They are set on the environment's own screen once it
// does — by an operator, or by an owner an operator named — which is also
// where they are changed afterwards, so there is one place for them rather
// than two.

const props = defineProps<{ project: string }>();
const emit = defineEmits<{ declared: [name: string] }>();

const toast = useToast();
const open = ref(false);
const name = ref("");
const declaring = ref(false);

// A name becomes a hostname, so the field accepts what a hostname label
// accepts. The API refuses the rest — a name the platform already serves, one
// shaped like a pull request's preview, one already taken — and says which,
// so nothing here guesses at those.
const ready = computed(() => /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(name.value.trim()));

async function declare() {
  if (!ready.value || declaring.value) return;
  declaring.value = true;
  try {
    const environment = await api.declareEnvironment(props.project, name.value.trim());
    open.value = false;
    name.value = "";
    toast.add({
      title: `Environment ${environment.name} declared`,
      description: "Nothing is deployed into it yet. The first build for it deploys here.",
      color: "success",
      icon: "i-lucide-check",
    });
    emit("declared", environment.name);
  } catch (err) {
    toast.add({
      title: "Declaring the environment failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    declaring.value = false;
  }
}
</script>

<template>
  <UModal
    v-model:open="open"
    title="Declare an environment"
    description="An environment that exists before anything is deployed into it — so what it demands can be set before the first release arrives, rather than after."
  >
    <slot>
      <UButton icon="i-lucide-plus" color="neutral" variant="subtle" size="sm">Declare an environment</UButton>
    </slot>

    <template #body>
      <form class="space-y-4" @submit.prevent="declare">
        <UFormField
          label="Name"
          help="Lowercase letters, digits and hyphens. It becomes the address this environment answers at, so it is unique across the platform."
          required
        >
          <UInput v-model="name" placeholder="shop-staging" class="w-full font-mono" autofocus />
        </UFormField>
        <p class="text-xs text-muted">
          Which rung this is — production, or a stage on the way to it — follows the project's promotion
          pipeline and is not asked here. Nothing is deployed until a build for this environment lands.
        </p>
        <p v-if="isOperator" class="text-xs text-muted">
          Its owners, the policy bundle it requires, its classification and its tolerances are set on the
          environment's own screen once it exists.
        </p>
        <p v-else class="text-xs text-muted">
          What it demands — its owners, the policy bundle it requires, its classification and its tolerances
          — is a platform operator's to declare, on the environment's own screen once it exists.
        </p>
      </form>
    </template>

    <template #footer>
      <div class="flex justify-end gap-2 w-full">
        <UButton color="neutral" variant="subtle" @click="open = false">Cancel</UButton>
        <UButton :disabled="!ready" :loading="declaring" icon="i-lucide-plus" @click="declare">Declare</UButton>
      </div>
    </template>
  </UModal>
</template>
