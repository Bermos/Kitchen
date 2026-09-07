<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type Claim, type Offering, type OfferingWrite, type Process } from "../lib/api";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";

// What this project offers other projects (#493).
//
// Kitchen's grouping answered "what does this project depend on" — a claim,
// a connection, an addon — and had nothing to say about the other half:
// another team's application calling this one. An offering is that half, and
// it is a list on the project rather than an object of its own, because it is
// a statement the project makes about itself and there is nothing to
// reconcile until somebody claims it.
//
// **The one field here that is not a description is `visible to`.** Which
// workload answers and what it speaks may also be declared in the
// repository's kitchen.json, which is why an offering can appear on this
// table without anybody having filled the form in. Who may bind is a grant,
// and a grant anybody with push access could widen is not a grant — so it is
// written here and refused there.
//
// The consumers column is the part worth showing rather than counting: the
// blast radius of withdrawing an offering is other people's applications, and
// the names are who has to be told.

const props = defineProps<{
  project: string;
  role?: string;
  offers?: Offering[];
  /** The project's workloads besides its web one, so an offering can name the
   * one that answers without anybody typing a name the platform will
   * refuse. */
  processes?: Process[];
  /** Every claim on the platform this account can see, which is where the
   * consumers of each offering are read from. */
  claims?: Claim[];
  /** The repository file that declares some of these, where one does. */
  declaredIn?: string;
  /** Which offerings that file names. */
  declaredNames?: string[];
}>();
const emit = defineEmits<{ saved: [] }>();

const toast = useToast();

const caller = computed(() => callerFor(props.role, props.project));
const mayEdit = computed(() => may("PATCH /api/v1/projects/{name}", caller.value));
const readOnlyReason = computed(() => refusal("PATCH /api/v1/projects/{name}", caller.value));

const offers = computed(() => props.offers ?? []);

/** Whether the repository declares this offering, and so sets its shape back
 * at every build. Said on the row, because that is where somebody is about to
 * change it. */
function fromRepository(name: string): boolean {
  return Boolean(props.declaredIn) && (props.declaredNames ?? []).includes(name);
}

/** Who binds to one offering: every project whose claim names it, other than
 * this one. It is read off the claims the account can already see, so a
 * viewer of fewer projects sees fewer names — which is the same rule every
 * cross-project read here follows. */
function consumers(name: string): string[] {
  const names = (props.claims ?? [])
    .filter(
      (claim) =>
        claim.type === "service" &&
        claim.service?.project === props.project &&
        claim.service?.offering === name &&
        claim.project !== props.project,
    )
    .map((claim) => claim.project);
  return [...new Set(names)].sort();
}

// The editor. One offering at a time in a dialogue, like the files panel: the
// fields are few but three of them are choices with consequences, and a row
// of selects in a table reads as though none of them matters.
const open = ref(false);
const editing = ref(-1);
const draft = ref<OfferingWrite>(blank());
const writeError = ref("");
const saving = ref(false);

function blank(): OfferingWrite {
  return { name: "", process: "web", protocol: "http", auth: "none", visibility: "request", environment: "" };
}

const adding = computed(() => editing.value < 0);
const nameProblem = computed(() => {
  const name = (draft.value.name ?? "").trim();
  if (!name) return "";
  if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name) || name.length > 40) {
    return "Lowercase letters, digits and dashes, at most 40 — the name travels into the consumer's variables.";
  }
  const taken = offers.value.some((offering, index) => offering.name === name && index !== editing.value);
  return taken ? "This project already offers something under that name." : "";
});
const complete = computed(() => Boolean((draft.value.name ?? "").trim()) && !nameProblem.value);

const workloadOptions = computed(() => [
  { label: "web — the published workload", value: "web" },
  ...(props.processes ?? [])
    .filter((process) => process.type === "service")
    .map((process) => ({ label: `${process.name} — a service workload`, value: process.name })),
]);

const protocolOptions = [
  { label: "http — the consumer is handed a URL, a host and a port", value: "http" },
  { label: "tcp — the consumer is handed a host and a port", value: "tcp" },
];

const visibilityOptions = [
  { label: "request — only projects this one has approved may bind", value: "request" },
  { label: "open — any project on the platform may bind", value: "open" },
];

function openEditor(at?: number) {
  editing.value = at ?? -1;
  writeError.value = "";
  if (at === undefined) {
    draft.value = blank();
  } else {
    const offering = offers.value[at];
    draft.value = {
      name: offering.name,
      process: offering.process,
      protocol: offering.protocol,
      auth: offering.auth,
      visibility: offering.visibility,
      environment: offering.environment,
    };
  }
  open.value = true;
}

/** The whole list as the API takes it: this route replaces it, so every
 * offering goes back with the one that changed. */
function writes(next: OfferingWrite[]): OfferingWrite[] {
  return next.map((offering) => ({
    name: (offering.name ?? "").trim(),
    process: offering.process,
    protocol: offering.protocol,
    auth: offering.auth,
    visibility: offering.visibility,
    ...(offering.environment ? { environment: offering.environment } : {}),
  }));
}

async function save() {
  if (!complete.value || saving.value) return;
  saving.value = true;
  writeError.value = "";
  const next = offers.value.map(
    (offering): OfferingWrite => ({
      name: offering.name,
      process: offering.process,
      protocol: offering.protocol,
      auth: offering.auth,
      visibility: offering.visibility,
      environment: offering.environment,
    }),
  );
  if (adding.value) next.push(draft.value);
  else next[editing.value] = draft.value;
  try {
    await api.updateProject(props.project, { offers: writes(next) });
    open.value = false;
    toast.add({
      title: `${(draft.value.name ?? "").trim()} offered`,
      description:
        draft.value.visibility === "open"
          ? "Any project on the platform may bind to it now."
          : "Nothing binds to it until this project admits a consumer.",
      color: "success",
      icon: "i-lucide-check",
    });
    emit("saved");
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
  } finally {
    saving.value = false;
  }
}

const removing = ref(-1);
const removed = ref(false);

async function remove() {
  if (removing.value < 0 || removed.value) return;
  removed.value = true;
  writeError.value = "";
  const next = offers.value
    .filter((_, index) => index !== removing.value)
    .map(
      (offering): OfferingWrite => ({
        name: offering.name,
        process: offering.process,
        protocol: offering.protocol,
        auth: offering.auth,
        visibility: offering.visibility,
        environment: offering.environment,
      }),
    );
  try {
    await api.updateProject(props.project, { offers: writes(next) });
    removing.value = -1;
    emit("saved");
  } catch (err) {
    removing.value = -1;
    writeError.value = err instanceof Error ? err.message : String(err);
  } finally {
    removed.value = false;
  }
}

// A dialogue left open while the project reloaded under it would be editing a
// row that is no longer there.
watch(
  () => props.project,
  () => {
    open.value = false;
    removing.value = -1;
  },
);
</script>

<template>
  <div class="space-y-4">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-sm font-medium text-highlighted">Offerings</h2>
        <p class="text-xs text-muted mt-1">
          What <span class="font-mono">{{ project }}</span> offers the other projects on this platform. A consumer
          claims one by name and is handed an address for it; nothing is published to the internet by offering
          something, and the workload behind it stays this project's.
        </p>
        <p class="text-xs text-dimmed mt-1">
          Which workload answers and what it speaks may also be declared in the repository. Who may bind is not: it is a
          grant, and it is set here.
        </p>
      </div>
      <UButton v-if="mayEdit" size="xs" color="neutral" variant="subtle" icon="i-lucide-plus" @click="openEditor()">
        Offer something
      </UButton>
    </div>

    <UAlert
      v-if="writeError && !open"
      color="warning"
      variant="soft"
      icon="i-lucide-info"
      :title="writeError"
      close
      @update:open="writeError = ''"
    />

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full min-w-[44rem] text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default">
            <th class="px-3 py-2 font-medium">Offering</th>
            <th class="px-3 py-2 font-medium">Answered by</th>
            <th class="px-3 py-2 font-medium">Speaks</th>
            <th class="px-3 py-2 font-medium">Visible to</th>
            <th class="px-3 py-2 font-medium">Bound by</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!offers.length">
            <td colspan="6" class="px-3 py-8 text-center text-muted">
              This project offers nothing. An application another team calls is offered here rather than published.
            </td>
          </tr>
          <tr v-for="(offering, index) in offers" :key="offering.name" class="border-b border-muted last:border-0">
            <td class="px-3 py-2">
              <p class="text-highlighted font-mono">{{ offering.name }}</p>
              <p class="text-xs text-dimmed mt-1">
                served from <span class="font-mono">{{ offering.environment }}</span>
              </p>
              <p v-if="fromRepository(offering.name)" class="text-xs text-dimmed mt-1">
                declared in <span class="font-mono">{{ declaredIn }}</span>
              </p>
            </td>
            <td class="px-3 py-2 font-mono text-muted">{{ offering.process }}</td>
            <td class="px-3 py-2 text-muted">{{ offering.protocol }}</td>
            <td class="px-3 py-2">
              <UBadge :color="offering.visibility === 'open' ? 'neutral' : 'neutral'" variant="subtle" size="sm">
                {{ offering.visibility }}
              </UBadge>
              <p v-if="offering.visibility === 'request'" class="text-xs text-dimmed mt-1">
                nothing binds until this project admits a consumer
              </p>
            </td>
            <td class="px-3 py-2">
              <template v-if="consumers(offering.name).length">
                <UBadge
                  v-for="consumer in consumers(offering.name)"
                  :key="consumer"
                  color="neutral"
                  variant="subtle"
                  size="sm"
                  class="font-mono mr-1"
                >
                  {{ consumer }}
                </UBadge>
              </template>
              <span v-else class="text-xs text-dimmed">nobody yet</span>
            </td>
            <td class="px-3 py-2 text-right whitespace-nowrap">
              <UButton
                v-if="mayEdit"
                color="neutral"
                variant="link"
                size="xs"
                class="px-0 mr-3"
                @click="openEditor(index)"
              >
                Edit
              </UButton>
              <UButton
                v-if="mayEdit"
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-trash-2"
                :aria-label="`Withdraw ${offering.name}`"
                @click="removing = index"
              />
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <p v-if="!mayEdit && readOnlyReason" class="text-xs text-dimmed">{{ readOnlyReason }}</p>

    <UModal v-model:open="open" :title="adding ? 'Offer something' : `Edit ${draft.name}`">
      <template #body>
        <div class="space-y-4">
          <UFormField label="Name" description="What a consumer's claim names, and the variable it arrives in.">
            <UInput v-model="draft.name" :disabled="!adding" placeholder="pricing-api" class="w-full font-mono" />
          </UFormField>
          <p v-if="nameProblem" class="text-xs text-error">{{ nameProblem }}</p>

          <UFormField label="Answered by" description="The workload a consumer reaches.">
            <USelect v-model="draft.process" :items="workloadOptions" class="w-full" />
          </UFormField>

          <UFormField label="Speaks" description="What the consumer is handed to reach it with.">
            <USelect v-model="draft.protocol" :items="protocolOptions" class="w-full" />
          </UFormField>

          <UFormField
            label="Visible to"
            description="Who may bind. This is the grant, and it is the one thing about an offering the repository cannot set."
          >
            <USelect v-model="draft.visibility" :items="visibilityOptions" class="w-full" />
          </UFormField>

          <UAlert
            v-if="draft.visibility === 'request'"
            color="neutral"
            variant="soft"
            icon="i-lucide-info"
            title="Nothing binds until this project admits it"
            description="A project asking for this offering waits, provisioning nothing, until an admin here answers it. The queue is on the project's own screen."
          />
          <UAlert
            v-if="!adding && draft.visibility === 'request' && offers[editing]?.visibility === 'open'"
            color="neutral"
            variant="soft"
            icon="i-lucide-info"
            title="The projects already bound stay bound"
            description="Closing an offering freezes new consumers. Each one already through is withdrawn on its own, from the requests on this project's screen."
          />
          <UAlert v-if="writeError" color="warning" variant="soft" icon="i-lucide-info" :title="writeError" />
        </div>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="ghost" @click="open = false">Cancel</UButton>
          <UButton color="primary" :loading="saving" :disabled="!complete" @click="save">Save</UButton>
        </div>
      </template>
    </UModal>

    <UModal
      :open="removing >= 0"
      :title="`Withdraw ${removing >= 0 ? offers[removing]?.name : ''}?`"
      @update:open="removing = -1"
    >
      <template #body>
        <p class="text-sm text-muted">
          The offering goes away and every binding to it stops resolving. Anything already bound keeps the address it
          has until its next deploy, and its claim then says the offering is gone.
        </p>
        <p v-if="removing >= 0 && consumers(offers[removing]?.name ?? '').length" class="text-sm text-warning mt-2">
          Bound by {{ consumers(offers[removing]?.name ?? "").join(", ") }}.
        </p>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="ghost" @click="removing = -1">Cancel</UButton>
          <UButton color="error" :loading="removed" @click="remove">Withdraw</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
