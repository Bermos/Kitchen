<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type IssuedPlatformCredential, type PlatformCredential } from "../lib/api";
import {
  credentialNameProblem,
  holdsNothing,
  LIFETIME_OPTIONS,
  scopeReach,
  scopeSummary,
} from "../lib/credentials";
import { timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";
import { useAsync } from "../lib/useAsync";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";

// The credentials this platform has issued to things that are not people.
//
// It is the project Keys panel one level up, and the difference is the whole
// point of the screen. A project's key holds a *role* on one project; one of
// these holds *scopes* on the platform — named operations, chosen when it is
// issued, reaching a short list of routes an operator would otherwise have to
// run by hand. The reason it is scopes and not a role is that the alternative
// is handing a scheduled job the operator hat, and the operator hat is
// everything, everywhere.
//
// So the screen is built around the three things that keep one of these small,
// and each is on the page rather than in the docs:
//
//   - **What a scope reaches** is listed from the API's own policy table, in
//     the words its refusals use. A list of scope names with nothing beside
//     them asks somebody to pick from a vocabulary they cannot evaluate.
//   - **When it expires** is not optional and is not "never". A credential
//     that outlives the job it was pasted into is the failure this design
//     exists to survive, so the form asks for a lifetime and the table shows
//     what is left of it.
//   - **What it holds now** — a lapsed credential, or one whose grant was
//     removed, holds nothing and says so. Both are answered by revoking it.
//
// It is a Platform-scope screen, so operator vocabulary is admissible here;
// what is on it is still the smallest thing that answers the question.

const toast = useToast();

const caller = computed(() => callerFor());
const mayIssue = computed(() => may("POST /api/v1/platform/credentials", caller.value));
const mayRevoke = computed(() => may("DELETE /api/v1/platform/credentials/{name}", caller.value));
const readOnlyReason = computed(() => refusal("POST /api/v1/platform/credentials", caller.value));

const { data, error, loading, refresh } = useAsync(() => api.platformCredentials());
const credentials = computed(() => data.value ?? []);
const reach = scopeReach();

// One line, in the API's own words, for whatever the last write ran into: a
// name already taken (409), a scope it does not know (400), or an installation
// federated to an issuer of its own, which issues no credentials at all (503).
const writeError = ref("");

// Issuing.
const issuing = ref(false);
const creating = ref(false);
const newName = ref("");
const newScopes = ref<string[]>([]);
const newDays = ref(30);
const newProjects = ref("");
const nameProblem = computed(() => (newName.value ? credentialNameProblem(newName.value) : ""));

function openIssue() {
  newName.value = "";
  newScopes.value = [];
  newDays.value = 30;
  newProjects.value = "";
  writeError.value = "";
  issuing.value = true;
}

function toggleScope(scope: string) {
  const held = newScopes.value;
  newScopes.value = held.includes(scope) ? held.filter((s) => s !== scope) : [...held, scope];
}

// The credential itself, held for exactly as long as the reveal is open.
const issued = ref<IssuedPlatformCredential | null>(null);
const copied = ref(false);
const copyFailed = ref(false);

async function create() {
  if (creating.value || credentialNameProblem(newName.value) || !newScopes.value.length) return;
  creating.value = true;
  writeError.value = "";
  try {
    const projects = newProjects.value
      .split(",")
      .map((name) => name.trim())
      .filter(Boolean);
    const credential = await api.createPlatformCredential({
      name: newName.value.trim(),
      scopes: newScopes.value,
      expiresInDays: newDays.value,
      ...(projects.length ? { projects } : {}),
    });
    issuing.value = false;
    copied.value = false;
    copyFailed.value = false;
    issued.value = credential;
    await refresh();
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
  } finally {
    creating.value = false;
  }
}

async function copyValue() {
  const value = issued.value?.key;
  if (!value) return;
  try {
    await navigator.clipboard.writeText(value);
    copied.value = true;
    copyFailed.value = false;
  } catch {
    copyFailed.value = true;
  }
}

/** Close the reveal, and forget the value. This is the only copy there will
 * ever be, and it does not stay in memory afterwards. */
function dismissIssued() {
  const name = issued.value?.name;
  issued.value = null;
  copied.value = false;
  copyFailed.value = false;
  if (name) {
    toast.add({
      title: `Credential ${name} issued`,
      description: "The value is not shown again — reissue the credential if it was not saved.",
      color: "success",
      icon: "i-lucide-key-round",
    });
  }
}

// Revoking.
const revoking = ref<PlatformCredential | null>(null);
const revoked = ref(false);
async function revoke() {
  const credential = revoking.value;
  if (!credential || revoked.value) return;
  revoked.value = true;
  writeError.value = "";
  try {
    await api.deletePlatformCredential(credential.name);
    toast.add({ title: `Credential ${credential.name} revoked`, color: "success", icon: "i-lucide-key-round" });
    revoking.value = null;
    await refresh();
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
    revoking.value = null;
  } finally {
    revoked.value = false;
  }
}

/** What a row says about a credential's life: how long it has left, or that it
 * has already run out. */
function lifetime(credential: PlatformCredential): string {
  if (!credential.expires) return "no expiry recorded";
  return credential.expired ? `expired ${timeAgo(credential.expires)}` : `expires ${timeAgo(credential.expires)}`;
}
</script>

<template>
  <div class="space-y-6">
    <PageHeader
      title="Credentials"
      :breadcrumb="[{ label: 'Platform', to: '/platform' }, { label: 'Credentials' }]"
    >
      <template #description>
        What this platform has handed to things that are not people — a scheduled job, an agent — and what each of them
        may do.
      </template>
      <template #actions>
        <UButton v-if="mayIssue" size="xs" color="neutral" variant="subtle" icon="i-lucide-plus" @click="openIssue">
          Issue a credential
        </UButton>
      </template>
    </PageHeader>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <UAlert
      v-if="writeError && !issuing"
      color="warning"
      variant="soft"
      icon="i-lucide-info"
      :title="writeError"
      close
      @update:open="writeError = ''"
    />

    <PageSection title="Issued">
      <template #description>
        A credential holds the scopes it was issued with and no role at all. It cannot issue another, change who the
        operators are, or create a project — and it stops working when it expires, whether or not anybody remembers it.
      </template>

      <div class="rounded-md border border-default overflow-x-auto">
        <table class="w-full min-w-[44rem] text-sm">
          <thead>
            <tr class="text-left text-xs text-muted border-b border-default">
              <th class="px-3 py-2 font-medium">Credential</th>
              <th class="px-3 py-2 font-medium">Scopes</th>
              <th class="px-3 py-2 font-medium">Life</th>
              <th class="px-3 py-2 font-medium">Last used</th>
              <th class="px-3 py-2"></th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="!credentials.length">
              <td colspan="5" class="px-3 py-8 text-center text-muted">
                {{ loading ? "Loading…" : "This platform has issued no credentials." }}
              </td>
            </tr>
            <tr v-for="credential in credentials" :key="credential.name" class="border-b border-muted last:border-0">
              <td class="px-3 py-2">
                <p class="text-highlighted font-mono">{{ credential.name }}</p>
                <p class="text-xs text-dimmed font-mono">
                  {{ credential.prefix }}… · issued {{ timeAgo(credential.created) }}
                </p>
              </td>
              <td class="px-3 py-2">
                <div v-if="credential.scopes.length" class="flex flex-wrap gap-1">
                  <UBadge
                    v-for="scope in credential.scopes"
                    :key="scope"
                    color="neutral"
                    variant="subtle"
                    size="sm"
                    class="font-mono"
                    :title="scopeSummary(scope)"
                  >
                    {{ scope }}
                  </UBadge>
                </div>
                <span
                  v-else
                  class="text-xs text-warning"
                  title="This credential's grant has been removed. It still authenticates and can do nothing."
                >
                  no scope — revoke it
                </span>
                <p v-if="credential.projects?.length" class="text-xs text-muted mt-1">
                  narrowed to {{ credential.projects.join(", ") }}
                </p>
              </td>
              <td class="px-3 py-2 text-xs" :class="credential.expired ? 'text-warning' : 'text-toned'">
                {{ lifetime(credential) }}
              </td>
              <td class="px-3 py-2 text-xs text-toned">
                {{ credential.lastUsed ? timeAgo(credential.lastUsed) : "never" }}
              </td>
              <td class="px-3 py-2 text-right whitespace-nowrap">
                <UButton
                  v-if="mayRevoke"
                  color="neutral"
                  variant="ghost"
                  size="xs"
                  icon="i-lucide-trash-2"
                  :aria-label="`Revoke ${credential.name}`"
                  @click="revoking = credential"
                />
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <p v-if="!mayIssue && readOnlyReason" class="text-xs text-muted mt-3">{{ readOnlyReason }}.</p>
      <p v-else-if="credentials.some((credential) => holdsNothing(credential))" class="text-xs text-muted mt-3">
        A credential that has lapsed or lost its grant holds nothing. Revoking it removes the account behind it; the
        platform does the same on its own within a few minutes of an expiry.
      </p>
    </PageSection>

    <PageSection title="What each scope reaches">
      <template #description>
        Read from the API's own table, in the words its refusals use. A route that names no scope is the operator's and
        cannot be reached by any credential, however it was issued.
      </template>
      <dl class="space-y-4">
        <div v-for="entry in reach" :key="entry.scope">
          <dt class="text-xs font-medium text-highlighted font-mono">{{ entry.scope }}</dt>
          <dd class="text-xs text-muted mt-1">
            <ul class="list-disc pl-4 space-y-0.5">
              <li v-for="operation in entry.operations" :key="operation">{{ operation }}</li>
            </ul>
          </dd>
        </div>
      </dl>
    </PageSection>

    <!-- Issuing: a name, the scopes, and how long it lasts. -->
    <UModal
      v-model:open="issuing"
      title="Issue a platform credential"
      description="The credential is created at the identity provider and given its scopes on the platform in one write. It is shown once, in the next dialogue."
    >
      <template #body>
        <form class="space-y-4" @submit.prevent="create">
          <UAlert v-if="writeError" color="warning" variant="soft" icon="i-lucide-info" :title="writeError" />
          <UFormField
            label="Name"
            help="Lowercase letters, digits and dashes. It is how the credential is revoked later, so name it after what holds it."
            :error="nameProblem || undefined"
          >
            <UInput v-model="newName" placeholder="nightly" class="w-full font-mono" autocomplete="off" />
          </UFormField>

          <UFormField
            label="Scopes"
            help="What this credential may do. Pick the narrowest set that works — reading and taking a backup are separate on purpose."
          >
            <div class="space-y-2">
              <label
                v-for="entry in reach"
                :key="entry.scope"
                class="flex items-start gap-2 rounded-md border border-default p-2 cursor-pointer"
              >
                <UCheckbox
                  :model-value="newScopes.includes(entry.scope)"
                  @update:model-value="toggleScope(entry.scope)"
                />
                <span>
                  <span class="text-sm font-mono text-highlighted">{{ entry.scope }}</span>
                  <span class="block text-xs text-muted">{{ entry.operations.join("; ") }}</span>
                </span>
              </label>
            </div>
          </UFormField>

          <UFormField
            label="Lifetime"
            help="A credential that has to outlive a quarter is one to reissue, which is an action somebody takes and the log records."
          >
            <USelect v-model="newDays" :items="LIFETIME_OPTIONS" class="w-full" />
          </UFormField>

          <UFormField
            label="Projects"
            help="Optional, comma separated. Narrows the scoped operations that are about one project — a project's audit pack. Left empty it is every project."
          >
            <UInput v-model="newProjects" placeholder="shop, billing" class="w-full font-mono" autocomplete="off" />
          </UFormField>
        </form>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="issuing = false">Cancel</UButton>
          <UButton
            :disabled="!newName.trim() || !!nameProblem || !newScopes.length"
            :loading="creating"
            icon="i-lucide-key-round"
            @click="create"
          >
            Issue credential
          </UButton>
        </div>
      </template>
    </UModal>

    <!-- The reveal. This is the only time the value exists outside the job it
         is going into, and closing the dialogue is what forgets it. -->
    <UModal
      :open="issued !== null"
      :title="`Credential ${issued?.name ?? ''}`"
      :dismissible="false"
      @update:open="(open: boolean) => { if (!open) dismissIssued(); }"
    >
      <template #body>
        <div class="space-y-4">
          <UAlert
            color="warning"
            variant="soft"
            icon="i-lucide-eye-off"
            title="This is the only time the credential is shown"
            description="It is stored hashed, so nothing can read it back — not this dashboard, not the API, not an operator. If it is lost, revoke this credential and issue another."
          />
          <div class="rounded-md border border-default bg-muted p-3 space-y-2">
            <p class="font-mono text-sm text-highlighted break-all select-all">{{ issued?.key }}</p>
            <div class="flex items-center gap-3">
              <UButton size="xs" color="neutral" variant="subtle" icon="i-lucide-copy" @click="copyValue">
                {{ copied ? "Copied" : "Copy" }}
              </UButton>
              <span v-if="copyFailed" class="text-xs text-muted">
                This browser would not hand over the clipboard — select the value above and copy it.
              </span>
            </div>
          </div>
          <p class="text-xs text-muted">
            Sign in with it — <span class="font-mono">kitchen login</span> — or exchange it at the platform's token
            endpoint. It holds <span class="font-mono">{{ issued?.scopes.join(", ") }}</span> and nothing else, and it
            stops working {{ issued?.expires ? timeAgo(issued.expires) : "when it expires" }}.
          </p>
        </div>
      </template>
      <template #footer>
        <div class="flex justify-end w-full">
          <UButton icon="i-lucide-check" @click="dismissIssued">I have saved it</UButton>
        </div>
      </template>
    </UModal>

    <UModal
      :open="revoking !== null"
      :title="`Revoke ${revoking?.name ?? ''}?`"
      description="The credential stops working immediately and its grant comes off the platform. Anything using it — a scheduled job, an agent — fails on its next call until it is given a new one."
      @update:open="(open: boolean) => { if (!open) revoking = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="revoking = null">Cancel</UButton>
          <UButton color="error" :loading="revoked" icon="i-lucide-trash-2" @click="revoke">Revoke credential</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
