<script setup lang="ts">
import { computed, ref, watch } from "vue";
import {
  changeName,
  changePassword,
  currentSession,
  expiresIn,
  hasPassword,
  listAccounts,
  listSessions,
  revokeSession,
  sessionRows,
  upstreamProviders,
  IssuerError,
  MAX_PASSWORD_LENGTH,
  MIN_PASSWORD_LENGTH,
} from "../lib/account";
import { api, type IssuedPersonalKey, type PersonalKey } from "../lib/api";
import { credentialNameProblem, LIFETIME_OPTIONS } from "../lib/credentials";
import { timeAgo } from "../lib/format";
import { useAsync } from "../lib/useAsync";
import PageHeader from "../components/PageHeader.vue";

// The account, as the person who owns it manages it: what it is called, how it
// signs in, and which browsers are signed in as it right now.
//
// Every call this screen makes goes to the *identity provider* rather than to
// the operator API — lib/account.ts says why, and what has to hold for a
// browser to be allowed to make one. What matters while reading this file is
// that those calls carry a different credential from every other screen in the
// dashboard (the issuer's session cookie, not the bearer token), so they fail
// in their own ways and are loaded in two pieces rather than one: an issuer
// that will not list sessions can still change a password, and a screen that
// blanked on the first refusal would offer neither.

const {
  data: identity,
  error: identityError,
  refresh: refreshIdentity,
} = useAsync(async () => {
  const [session, accounts] = await Promise.all([currentSession(), listAccounts()]);
  return { account: session.account, token: session.token, accounts };
});

const {
  data: sessionList,
  error: sessionsError,
  loading: sessionsLoading,
  refresh: refreshSessions,
} = useAsync(() => listSessions());

const account = computed(() => identity.value?.account ?? null);
const accounts = computed(() => identity.value?.accounts ?? []);
const password = computed(() => hasPassword(accounts.value));
const upstream = computed(() => upstreamProviders(accounts.value));
const rows = computed(() => sessionRows(sessionList.value ?? [], identity.value?.token ?? null));

/** How this account can sign in, in words: what the profile card reports. */
const methods = computed(() => [...(password.value ? ["a password"] : []), ...upstream.value].join(", ") || "—");

const toast = useToast();

// What a write to the issuer failed with, kept next to the form that made it
// rather than at the top of the page: the three forms here fail for three
// unrelated reasons, and one shared line would attribute the wrong one.
const nameError = ref("");
const passwordError = ref("");
const sessionError = ref("");

// --- the display name -------------------------------------------------------

const name = ref("");
// Prefilled from the issuer rather than from the access token: the token's
// copy was stamped at sign-in, so it is the one thing on this screen that is
// allowed to be out of date, and it is not the one to edit from.
watch(account, (value) => {
  if (value) name.value = value.name;
});
const nameChanged = computed(() => Boolean(account.value) && name.value.trim() !== account.value?.name);
const savingName = ref(false);

async function saveName() {
  const wanted = name.value.trim();
  if (!wanted || !nameChanged.value) {
    nameError.value = wanted ? "" : "a name cannot be empty";
    return;
  }
  savingName.value = true;
  nameError.value = "";
  try {
    await changeName(wanted);
    toast.add({ title: "Name changed", color: "success", icon: "i-lucide-check" });
    await refreshIdentity();
  } catch (err) {
    nameError.value = err instanceof IssuerError ? err.message : String(err);
  } finally {
    savingName.value = false;
  }
}

// --- the password -----------------------------------------------------------

const currentPassword = ref("");
const newPassword = ref("");
const confirmPassword = ref("");
const revokeOthers = ref(false);
const changingPassword = ref(false);

/**
 * What is wrong with the form as typed, or empty while there is nothing to
 * say. Checked here as well as at the issuer because the issuer never sees the
 * confirmation field at all — a mistyped one has to cost a glance rather than a
 * round trip that would change the password to something nobody knows.
 */
const passwordProblem = computed(() => {
  if (!currentPassword.value || !newPassword.value) return "";
  if (newPassword.value.length < MIN_PASSWORD_LENGTH) {
    return `the new password must be at least ${MIN_PASSWORD_LENGTH} characters`;
  }
  if (newPassword.value.length > MAX_PASSWORD_LENGTH) {
    return `the new password must be at most ${MAX_PASSWORD_LENGTH} characters`;
  }
  if (newPassword.value === currentPassword.value) return "the new password is the one already set";
  if (confirmPassword.value && confirmPassword.value !== newPassword.value) {
    return "the two new passwords are not the same";
  }
  return "";
});

const passwordReady = computed(
  () => Boolean(currentPassword.value && newPassword.value && confirmPassword.value) && !passwordProblem.value,
);

async function submitPassword() {
  if (!passwordReady.value) return;
  changingPassword.value = true;
  passwordError.value = "";
  const revoked = revokeOthers.value;
  try {
    await changePassword(currentPassword.value, newPassword.value, revoked);
    toast.add({
      title: "Password changed",
      description: revoked ? "Every other signed-in browser has been signed out." : undefined,
      color: "success",
      icon: "i-lucide-check",
    });
    currentPassword.value = "";
    newPassword.value = "";
    confirmPassword.value = "";
    revokeOthers.value = false;
    // The list is stale either way: the issuer mints this browser a new
    // session as part of the change, whether or not the others were ended.
    await refreshSessions();
  } catch (err) {
    passwordError.value = err instanceof IssuerError ? err.message : String(err);
  } finally {
    changingPassword.value = false;
  }
}

// --- personal keys ----------------------------------------------------------

// The one thing on this screen that is the *operator API's* rather than the
// identity provider's, and it is here because it belongs beside the sessions:
// both answer "what is signed in as me right now", one for browsers and one
// for scripts.
//
// Issuing is only possible from a signed-in browser — the API asks for a token
// issued to this dashboard's own OAuth client, which is exactly what this page
// is holding — so this screen is not one surface among several. It is the
// surface. Everything else about a personal key (list, revoke) works from the
// CLI as well.

const {
  data: keyList,
  error: keysError,
  loading: keysLoading,
  refresh: refreshKeys,
} = useAsync(() => api.personalKeys());
const keys = computed(() => keyList.value ?? []);

/** One line, in the API's own words, for whatever the last write ran into. */
const keyError = ref("");

const issuing = ref(false);
const creating = ref(false);
const newKeyName = ref("");
const newKeyDays = ref(30);
const keyNameProblem = computed(() => (newKeyName.value ? credentialNameProblem(newKeyName.value) : ""));

function openIssue() {
  newKeyName.value = "";
  newKeyDays.value = 30;
  keyError.value = "";
  issuing.value = true;
}

/** The key itself, held for exactly as long as the reveal is open. */
const issued = ref<IssuedPersonalKey | null>(null);
const copied = ref(false);
const copyFailed = ref(false);

async function createKey() {
  if (creating.value || credentialNameProblem(newKeyName.value)) return;
  creating.value = true;
  keyError.value = "";
  try {
    const key = await api.createPersonalKey({
      name: newKeyName.value.trim(),
      expiresInDays: newKeyDays.value,
    });
    issuing.value = false;
    copied.value = false;
    copyFailed.value = false;
    issued.value = key;
    await refreshKeys();
  } catch (err) {
    keyError.value = err instanceof Error ? err.message : String(err);
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
      title: `Personal key ${name} issued`,
      description: "The value is not shown again — revoke it and make another if it was not saved.",
      color: "success",
      icon: "i-lucide-key-round",
    });
  }
}

const revokingKey = ref<PersonalKey | null>(null);
const revokedKey = ref(false);

async function revokeKey() {
  const key = revokingKey.value;
  if (!key || revokedKey.value) return;
  revokedKey.value = true;
  keyError.value = "";
  try {
    await api.deletePersonalKey(key.name);
    toast.add({ title: `Personal key ${key.name} revoked`, color: "success", icon: "i-lucide-key-round" });
    revokingKey.value = null;
    await refreshKeys();
  } catch (err) {
    keyError.value = err instanceof Error ? err.message : String(err);
    revokingKey.value = null;
  } finally {
    revokedKey.value = false;
  }
}

/** What a row says about a key's life: how long it has left, or that it has
 * already run out. */
function keyLifetime(key: PersonalKey): string {
  if (!key.expires) return "no expiry recorded";
  return key.expired ? `expired ${timeAgo(key.expires)}` : `expires ${timeAgo(key.expires)}`;
}

// --- the sessions -----------------------------------------------------------

const revoking = ref("");

async function revoke(token: string) {
  revoking.value = token;
  sessionError.value = "";
  try {
    await revokeSession(token);
    toast.add({ title: "Browser signed out", color: "success", icon: "i-lucide-check" });
    await refreshSessions();
  } catch (err) {
    sessionError.value = err instanceof IssuerError ? err.message : String(err);
  } finally {
    revoking.value = "";
  }
}
</script>

<template>
  <div class="space-y-6 max-w-3xl">
    <PageHeader title="Account">
      <template #description>
        Your own account at the platform's identity provider — what it is called, how it signs in, and which browsers
        are signed in as it. Roles are granted on a project, not here.
      </template>
    </PageHeader>

    <UAlert v-if="identityError" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="identityError" />

    <template v-if="account">
      <!-- Profile -->
      <section class="rounded-md border border-default p-4 space-y-4">
        <div>
          <h2 class="text-sm font-medium text-highlighted">Profile</h2>
          <p class="text-xs text-muted mt-1">
            The name everything you create on this platform is attributed to. The account menu keeps showing the old
            one until you sign in again: it reads the name out of the access token, which was stamped at sign-in.
          </p>
        </div>

        <UAlert v-if="nameError" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="nameError" />

        <div class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Name">
            <UInput v-model="name" class="w-full" autocomplete="name" @keyup.enter="saveName" />
          </UFormField>
          <UFormField label="Email" :help="`Signs in with ${methods}.`">
            <UInput :model-value="account.email" class="w-full font-mono" disabled />
          </UFormField>
        </div>

        <UButton size="sm" color="primary" :loading="savingName" :disabled="!nameChanged" @click="saveName">
          Save
        </UButton>

        <!-- Being told what is missing beats finding out by typing into a field
             that then refuses. Both of these need the platform to be able to
             send mail to prove an address, and it has no way to send any. -->
        <p class="text-xs text-dimmed border-t border-muted pt-3">
          Changing the address, and recovering a forgotten password, both need the platform to send mail — and Kitchen
          ships no mail transport, so neither exists. A forgotten password is reset by an operator at the identity
          provider; docs/AUTH.md says how.
        </p>
      </section>

      <!-- Password -->
      <section class="rounded-md border border-default p-4 space-y-4">
        <div>
          <h2 class="text-sm font-medium text-highlighted">Password</h2>
          <p class="text-xs text-muted mt-1">
            Changing it proves the current one. The browsers already signed in stay signed in unless you say otherwise.
          </p>
        </div>

        <UAlert
          v-if="!password"
          color="neutral"
          variant="soft"
          icon="i-lucide-key-round"
          title="This account has no password"
          :description="`It signs in through ${upstream.join(', ') || 'an upstream provider'}, so the password lives there rather than here.`"
        />

        <template v-else>
          <UAlert
            v-if="passwordError"
            color="error"
            variant="soft"
            icon="i-lucide-triangle-alert"
            :title="passwordError"
          />

          <UFormField label="Current password" required class="sm:max-w-xs">
            <UInput v-model="currentPassword" type="password" class="w-full" autocomplete="current-password" />
          </UFormField>

          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="New password" required :help="`At least ${MIN_PASSWORD_LENGTH} characters.`">
              <UInput v-model="newPassword" type="password" class="w-full" autocomplete="new-password" />
            </UFormField>
            <UFormField label="New password again" required>
              <UInput
                v-model="confirmPassword"
                type="password"
                class="w-full"
                autocomplete="new-password"
                @keyup.enter="submitPassword"
              />
            </UFormField>
          </div>

          <USwitch
            v-model="revokeOthers"
            label="Sign out every other browser"
            description="For a password changed because somebody else may know it."
          />

          <div class="flex items-center gap-3 flex-wrap">
            <UButton
              size="sm"
              color="primary"
              :loading="changingPassword"
              :disabled="!passwordReady"
              @click="submitPassword"
            >
              Change password
            </UButton>
            <span v-if="passwordProblem" class="text-xs text-warning">{{ passwordProblem }}</span>
          </div>
        </template>
      </section>

      <!-- Personal keys -->
      <section class="rounded-md border border-default p-4 space-y-4">
        <div class="flex items-start justify-between gap-4">
          <div>
            <h2 class="text-sm font-medium text-highlighted">Personal keys</h2>
            <p class="text-xs text-muted mt-1">
              A credential that is you: every project role you hold, and the operator role if you have it. It is what a
              script needs to do what you would do — and it is issued here because only a signed-in browser may make
              one. A credential cannot mint another.
            </p>
          </div>
          <UButton size="xs" color="neutral" variant="subtle" icon="i-lucide-plus" @click="openIssue">
            Issue a key
          </UButton>
        </div>

        <UAlert v-if="keysError" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="keysError" />
        <UAlert
          v-if="keyError && !issuing"
          color="warning"
          variant="soft"
          icon="i-lucide-info"
          :title="keyError"
          close
          @update:open="keyError = ''"
        />

        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[34rem] text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default">
                <th class="px-3 py-2 font-medium">Key</th>
                <th class="px-3 py-2 font-medium">Life</th>
                <th class="px-3 py-2 font-medium">Last used</th>
                <th class="px-3 py-2"></th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!keys.length">
                <td colspan="4" class="px-3 py-8 text-center text-muted">
                  {{ keysLoading ? "Loading…" : "No personal keys." }}
                </td>
              </tr>
              <tr v-for="key in keys" :key="key.name" class="border-b border-muted last:border-0">
                <td class="px-3 py-2">
                  <p class="text-highlighted font-mono">{{ key.name }}</p>
                  <p class="text-xs text-dimmed font-mono">{{ key.prefix }}… · made {{ timeAgo(key.created) }}</p>
                </td>
                <td class="px-3 py-2 text-xs" :class="key.expired ? 'text-warning' : 'text-toned'">
                  {{ keyLifetime(key) }}
                </td>
                <td class="px-3 py-2 text-xs text-toned">
                  {{ key.lastUsed ? timeAgo(key.lastUsed) : "never" }}
                </td>
                <td class="px-3 py-2 text-right whitespace-nowrap">
                  <UButton
                    color="neutral"
                    variant="ghost"
                    size="xs"
                    icon="i-lucide-trash-2"
                    :aria-label="`Revoke ${key.name}`"
                    @click="revokingKey = key"
                  />
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <p class="text-xs text-dimmed border-t border-muted pt-3">
          A personal key is as powerful as you are, so it expires — ninety days at the most — and it is listed here
          until it is revoked. For something narrower, a project's Keys panel issues a key that can deploy one project,
          and Platform → Credentials issues one scoped to named operations and no role at all.
        </p>
      </section>

      <!-- Sessions -->
      <section class="rounded-md border border-default p-4 space-y-4">
        <div class="flex items-start justify-between gap-4">
          <div>
            <h2 class="text-sm font-medium text-highlighted">Signed-in browsers</h2>
            <p class="text-xs text-muted mt-1">
              Every session the identity provider holds for this account. Signing one out ends it there, which is what
              stops that browser signing back in without the password.
            </p>
          </div>
          <UButton
            icon="i-lucide-refresh-cw"
            color="neutral"
            variant="ghost"
            size="sm"
            :loading="sessionsLoading"
            aria-label="Refresh"
            @click="refreshSessions"
          />
        </div>

        <UAlert
          v-if="sessionsError"
          color="error"
          variant="soft"
          icon="i-lucide-triangle-alert"
          :title="sessionsError"
        />
        <UAlert
          v-if="sessionError"
          color="error"
          variant="soft"
          icon="i-lucide-triangle-alert"
          :title="sessionError"
          close
          @update:open="sessionError = ''"
        />

        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[34rem] text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default">
                <th class="px-3 py-2 font-medium">Browser</th>
                <th class="px-3 py-2 font-medium">Signed in</th>
                <th class="px-3 py-2 font-medium">Expires</th>
                <th class="px-3 py-2"></th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!rows.length">
                <td colspan="4" class="px-3 py-8 text-center text-muted">
                  {{ sessionsLoading ? "Loading…" : "No sessions to show." }}
                </td>
              </tr>
              <tr v-for="row in rows" :key="row.id" class="border-b border-muted last:border-0">
                <td class="px-3 py-2">
                  <p class="text-highlighted flex items-center gap-2">
                    {{ row.device }}
                    <UBadge v-if="row.current" color="primary" variant="subtle" size="sm">this browser</UBadge>
                  </p>
                  <p class="text-xs text-dimmed font-mono">{{ row.ipAddress || "no address recorded" }}</p>
                </td>
                <td class="px-3 py-2 text-xs text-toned">{{ timeAgo(row.createdAt) }}</td>
                <td class="px-3 py-2 text-xs text-toned">{{ expiresIn(row.expiresAt) }}</td>
                <td class="px-3 py-2 text-right whitespace-nowrap">
                  <!-- This browser gets no button on purpose: ending its own
                       session from here would sign the reader out mid-sentence,
                       and the account menu already does that deliberately. -->
                  <UButton
                    v-if="!row.current"
                    size="xs"
                    color="error"
                    variant="subtle"
                    :loading="revoking === row.token"
                    @click="revoke(row.token)"
                  >
                    Sign out
                  </UButton>
                  <span v-else class="text-xs text-dimmed">Sign out is in the account menu</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
    </template>

    <!-- Issuing: a name, and how long it lasts. There is nothing else to ask:
         a personal key holds what its owner holds, so there is no scope to
         pick and no role to narrow. -->
    <UModal
      v-model:open="issuing"
      title="Issue a personal key"
      description="It carries your own identity — every role you hold — so it does what you would do. It is shown once, in the next dialogue."
    >
      <template #body>
        <form class="space-y-4" @submit.prevent="createKey">
          <UAlert v-if="keyError" color="warning" variant="soft" icon="i-lucide-info" :title="keyError" />
          <UFormField
            label="Name"
            help="Lowercase letters, digits and dashes. It is how you revoke it later, so name it after what will hold it — laptop, nightly, the pipeline."
            :error="keyNameProblem || undefined"
          >
            <UInput v-model="newKeyName" placeholder="laptop" class="w-full font-mono" autocomplete="off" />
          </UFormField>

          <UFormField
            label="Lifetime"
            help="A key that carries every role you hold and has to outlive a quarter is one to reissue, which is an action somebody takes and the log records."
          >
            <USelect v-model="newKeyDays" :items="LIFETIME_OPTIONS" class="w-full" />
          </UFormField>

          <p class="text-xs text-muted">
            Anything holding this key can do anything you can, on every project you are on. Give it to a script you
            control, not to a service somebody else runs.
          </p>
        </form>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="issuing = false">Cancel</UButton>
          <UButton
            :disabled="!newKeyName.trim() || !!keyNameProblem"
            :loading="creating"
            icon="i-lucide-key-round"
            @click="createKey"
          >
            Issue key
          </UButton>
        </div>
      </template>
    </UModal>

    <!-- The reveal. This is the only time the value exists outside the script
         it is going into, and closing the dialogue is what forgets it. -->
    <UModal
      :open="issued !== null"
      :title="`Personal key ${issued?.name ?? ''}`"
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
            description="It is stored hashed, so nothing can read it back — not this dashboard, not the API, not an operator. If it is lost, revoke it and make another."
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
            Sign in with it — <span class="font-mono">kitchen login --api-key-stdin</span> — or exchange it at the
            platform's token endpoint the way CI does. It stops working
            {{ issued?.expires ? timeAgo(issued.expires) : "when it expires" }}.
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
      :open="revokingKey !== null"
      :title="`Revoke ${revokingKey?.name ?? ''}?`"
      description="The key stops working immediately. Anything using it — a script, a pipeline, a laptop — fails on its next call until it is given another."
      @update:open="(open: boolean) => { if (!open) revokingKey = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="revokingKey = null">Cancel</UButton>
          <UButton color="error" :loading="revokedKey" icon="i-lucide-trash-2" @click="revokeKey">Revoke key</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
