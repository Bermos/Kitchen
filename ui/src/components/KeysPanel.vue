<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type IssuedKey, type ProjectKey } from "../lib/api";
import { timeAgo } from "../lib/format";
import { KEY_ROLE_OPTIONS, keyIsUngranted, keyNameProblem } from "../lib/members";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";
import { useAsync } from "../lib/useAsync";

// A project's CI keys, next to its people, because they are the same list: a
// key is a non-human member of exactly one project. It is owned by a machine
// account created for it — the identity provider mints a key's session for the
// account the key belongs to, so a key with no owner of its own would be a
// grant to whoever created it — and issuing one writes the credential and the
// grant together. The dashboard has nothing clever to do about that; what it
// does have to get right is the value.
//
// **The key is in the create response and in no other.** It is stored hashed,
// exactly as every other key at the issuer is, so a lost key is revoked and
// reissued rather than looked up. So the reveal says plainly that this is the
// only time it is shown, offers it to the clipboard, and drops it from this
// component the moment the dialogue closes — there is no reopening it, and a
// value still sitting in a closed component is a value the next render could
// put back on the screen.
//
// Reading the list is a viewer's, like reading the membership list it belongs
// to; issuing and revoking are an admin's, because they are adding and
// removing a member.

const props = defineProps<{ project: string; role?: string }>();

const toast = useToast();

const caller = computed(() => callerFor(props.role, props.project));
const mayIssue = computed(() => may("POST /api/v1/projects/{name}/keys", caller.value));
const mayRevoke = computed(() => may("DELETE /api/v1/projects/{name}/keys/{key}", caller.value));
const readOnlyReason = computed(() => refusal("POST /api/v1/projects/{name}/keys", caller.value));

const { data, error, loading, refresh } = useAsync(() => api.projectKeys(props.project));
watch(
  () => props.project,
  () => void refresh(),
);
const keys = computed(() => data.value ?? []);

// One line, in the API's own words, for whatever the last write ran into: a
// name already taken (409), a name the API spells its own rules for (400), or
// an installation federated to an issuer of its own, which serves no key
// endpoints at all (503). None of those is guessable from a dismissed toast.
const writeError = ref("");

// Issuing.
const issuing = ref(false);
const creating = ref(false);
const newName = ref("");
const newRole = ref("developer");
const roleOptions = KEY_ROLE_OPTIONS;
// The API's own name rule, checked here so a capital letter is a line under
// the field rather than a round trip. The API still decides.
const nameProblem = computed(() => (newName.value ? keyNameProblem(newName.value) : ""));

function openIssue() {
  newName.value = "";
  newRole.value = "developer";
  writeError.value = "";
  issuing.value = true;
}

// The key itself, held for exactly as long as the reveal is open.
const issued = ref<IssuedKey | null>(null);
const copied = ref(false);
const copyFailed = ref(false);

async function create() {
  if (creating.value || keyNameProblem(newName.value)) return;
  creating.value = true;
  writeError.value = "";
  try {
    const key = await api.createKey(props.project, { name: newName.value.trim(), role: newRole.value });
    issuing.value = false;
    copied.value = false;
    copyFailed.value = false;
    issued.value = key;
    await refresh();
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
  } finally {
    creating.value = false;
  }
}

async function copyKey() {
  const value = issued.value?.key;
  if (!value) return;
  try {
    await navigator.clipboard.writeText(value);
    copied.value = true;
    copyFailed.value = false;
  } catch {
    // A browser that will not hand out the clipboard — an installation served
    // over plain http, most likely. The value is on the screen and selectable,
    // so say that rather than failing silently.
    copyFailed.value = true;
  }
}

/** Close the reveal, and forget the key. Both halves matter: this is the only
 * copy there will ever be, and it does not stay in memory afterwards. */
function dismissIssued() {
  const name = issued.value?.name;
  issued.value = null;
  copied.value = false;
  copyFailed.value = false;
  if (name) {
    toast.add({
      title: `Key ${name} issued`,
      description: "The value is not shown again — reissue the key if it was not saved.",
      color: "success",
      icon: "i-lucide-key-round",
    });
  }
}

// Revoking.
const revoking = ref<ProjectKey | null>(null);
const revoked = ref(false);
async function revoke() {
  const key = revoking.value;
  if (!key || revoked.value) return;
  revoked.value = true;
  writeError.value = "";
  try {
    await api.deleteKey(props.project, key.name);
    toast.add({ title: `Key ${key.name} revoked`, color: "success", icon: "i-lucide-key-round" });
    revoking.value = null;
    await refresh();
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
    revoking.value = null;
  } finally {
    revoked.value = false;
  }
}
</script>

<template>
  <div class="space-y-4 max-w-3xl">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-sm font-medium text-highlighted">CI keys</h2>
        <p class="text-xs text-muted mt-1">
          A key is a member of <span class="font-mono">{{ project }}</span> and nothing else — it holds a role on this
          project the way a person does, so a key that can trigger a build cannot change the platform. Values are never
          read back; a key is shown once, when it is issued.
        </p>
      </div>
      <UButton v-if="mayIssue" size="xs" color="neutral" variant="subtle" icon="i-lucide-plus" @click="openIssue">
        Issue a key
      </UButton>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <!-- Shown here when the write that failed was made from this panel, and
         inside the dialogue when it was made from there — a refusal belongs
         where the control that caused it is, and the issue form is a modal
         that would cover this line. -->
    <UAlert
      v-if="writeError && !issuing"
      color="warning"
      variant="soft"
      icon="i-lucide-info"
      :title="writeError"
      close
      @update:open="writeError = ''"
    />

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full min-w-[34rem] text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default">
            <th class="px-3 py-2 font-medium">Key</th>
            <th class="px-3 py-2 font-medium">Role</th>
            <th class="px-3 py-2 font-medium">Last used</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!keys.length">
            <td colspan="4" class="px-3 py-8 text-center text-muted">
              {{ loading ? "Loading…" : "No CI keys on this project." }}
            </td>
          </tr>
          <tr v-for="key in keys" :key="key.name" class="border-b border-muted last:border-0">
            <td class="px-3 py-2">
              <p class="text-highlighted font-mono">{{ key.name }}</p>
              <p class="text-xs text-dimmed font-mono">{{ key.prefix }}… · issued {{ timeAgo(key.created) }}</p>
            </td>
            <td class="px-3 py-2">
              <UBadge v-if="!keyIsUngranted(key)" color="neutral" variant="subtle" size="sm" class="font-mono">
                {{ key.role }}
              </UBadge>
              <span
                v-else
                class="text-xs text-warning"
                title="This key's grant has been removed from the project. It still authenticates and can do nothing."
              >
                no role — revoke it
              </span>
            </td>
            <td class="px-3 py-2 text-xs text-toned">{{ key.lastUsed ? timeAgo(key.lastUsed) : "never" }}</td>
            <td class="px-3 py-2 text-right whitespace-nowrap">
              <UButton
                v-if="mayRevoke"
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-trash-2"
                :aria-label="`Revoke ${key.name}`"
                @click="revoking = key"
              />
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <p v-if="!mayIssue && readOnlyReason" class="text-xs text-muted">{{ readOnlyReason }}.</p>

    <!-- Issuing: a name and a role, and the role list is short on purpose. -->
    <UModal
      v-model:open="issuing"
      title="Issue a CI key"
      description="The key is created at the identity provider and given a role on this project in one write. It is shown once, in the next dialogue."
    >
      <template #body>
        <form class="space-y-4" @submit.prevent="create">
          <UAlert v-if="writeError" color="warning" variant="soft" icon="i-lucide-info" :title="writeError" />
          <UFormField
            label="Name"
            help="Lowercase letters, digits and dashes. It is how the key is revoked later, so name it after what uses it."
            :error="nameProblem || undefined"
          >
            <UInput v-model="newName" placeholder="nightly" class="w-full font-mono" autocomplete="off" />
          </UFormField>
          <UFormField
            label="Role"
            help="A key may be a developer or a viewer. Admin is refused: a credential in a pipeline that can issue its own successors is one nobody can account for."
          >
            <USelect v-model="newRole" :items="roleOptions" class="w-full" />
          </UFormField>
        </form>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="issuing = false">Cancel</UButton>
          <UButton
            :disabled="!newName.trim() || !!nameProblem"
            :loading="creating"
            icon="i-lucide-key-round"
            @click="create"
          >
            Issue key
          </UButton>
        </div>
      </template>
    </UModal>

    <!-- The reveal. This is the only time the value exists outside the
         pipeline it is going into, and closing the dialogue is what forgets
         it. -->
    <UModal
      :open="issued !== null"
      :title="`Key ${issued?.name ?? ''}`"
      :dismissible="false"
      @update:open="(open: boolean) => { if (!open) dismissIssued(); }"
    >
      <template #body>
        <div class="space-y-4">
          <UAlert
            color="warning"
            variant="soft"
            icon="i-lucide-eye-off"
            title="This is the only time the key is shown"
            description="It is stored hashed, so nothing can read it back — not this dashboard, not the API, not an operator. If it is lost, revoke this key and issue another."
          />
          <div class="rounded-md border border-default bg-muted p-3 space-y-2">
            <p class="font-mono text-sm text-highlighted break-all select-all">{{ issued?.key }}</p>
            <div class="flex items-center gap-3">
              <UButton size="xs" color="neutral" variant="subtle" icon="i-lucide-copy" @click="copyKey">
                {{ copied ? "Copied" : "Copy" }}
              </UButton>
              <span v-if="copyFailed" class="text-xs text-muted">
                This browser would not hand over the clipboard — select the value above and copy it.
              </span>
            </div>
          </div>
          <p class="text-xs text-muted">
            Send it as <span class="font-mono">authorization: Bearer</span> to the platform's token endpoint, or give it
            to your pipeline as a secret. It holds <span class="font-mono">{{ issued?.role }}</span> on
            <span class="font-mono">{{ project }}</span> and no role anywhere else.
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
      description="The credential stops working immediately and its grant comes off the project. Anything using it — a pipeline, a script — fails on its next call until it is given a new key."
      @update:open="(open: boolean) => { if (!open) revoking = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="revoking = null">Cancel</UButton>
          <UButton color="error" :loading="revoked" icon="i-lucide-trash-2" @click="revoke">Revoke key</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
