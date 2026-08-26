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
import { timeAgo } from "../lib/format";
import { useAsync } from "../lib/useAsync";

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
  <div class="space-y-5 max-w-3xl">
    <div>
      <h1 class="text-xl font-semibold text-highlighted">Account</h1>
      <p class="text-xs text-muted mt-1">
        Your own account at the platform's identity provider — what it is called, how it signs in, and which browsers
        are signed in as it. Roles are granted on a project, not here.
      </p>
    </div>

    <UAlert v-if="identityError" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="identityError" />

    <template v-if="account">
      <!-- Profile -->
      <section class="rounded-md border border-default p-4 space-y-4">
        <div>
          <h2 class="text-sm font-semibold text-highlighted">Profile</h2>
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
          <h2 class="text-sm font-semibold text-highlighted">Password</h2>
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

      <!-- Sessions -->
      <section class="rounded-md border border-default p-4 space-y-4">
        <div class="flex items-start justify-between gap-4">
          <div>
            <h2 class="text-sm font-semibold text-highlighted">Signed-in browsers</h2>
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
                <th class="px-4 py-2 font-medium">Browser</th>
                <th class="px-4 py-2 font-medium">Signed in</th>
                <th class="px-4 py-2 font-medium">Expires</th>
                <th class="px-4 py-2"></th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!rows.length">
                <td colspan="4" class="px-4 py-8 text-center text-muted">
                  {{ sessionsLoading ? "Loading…" : "No sessions to show." }}
                </td>
              </tr>
              <tr v-for="row in rows" :key="row.id" class="border-b border-muted last:border-0">
                <td class="px-4 py-3">
                  <p class="text-highlighted flex items-center gap-2">
                    {{ row.device }}
                    <UBadge v-if="row.current" color="primary" variant="subtle" size="sm">this browser</UBadge>
                  </p>
                  <p class="text-xs text-dimmed font-mono">{{ row.ipAddress || "no address recorded" }}</p>
                </td>
                <td class="px-4 py-3 text-xs text-toned">{{ timeAgo(row.createdAt) }}</td>
                <td class="px-4 py-3 text-xs text-toned">{{ expiresIn(row.expiresAt) }}</td>
                <td class="px-4 py-3 text-right whitespace-nowrap">
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
  </div>
</template>
