<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type Member } from "../lib/api";
import { memberDetail, memberKind, memberLabel, roleOptionsFor, MEMBER_ROLE_OPTIONS, ROLE_SUMMARY } from "../lib/members";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";
import { useAsync } from "../lib/useAsync";

// A project's people, on the project: who holds which role, adding somebody
// by the address they sign in with, moving them to another role, and taking
// them off. It is the rest of the sentence self-service starts — a project's
// creator is its admin the moment it exists, and this is how they hand it to
// anybody else without going through the platform's owner.
//
// **Reading the list is every member's; changing it is an admin's.** Knowing
// who else is on a project is part of knowing what the project is, so a viewer
// who opened this panel and was refused on load would be reading a screen
// about a project they can otherwise see in full. What a viewer gets is
// therefore this list and no controls — the roles as badges rather than as
// selects with nothing behind them, which is the "gone, not disabled" rule the
// rest of the dashboard follows.
//
// Keys are in this list too, because a CI key is a member of exactly one
// project: it is owned by a machine account created for it, and that account's
// subject is what the grant names. It reads as a key rather than as a stranger
// with an odd address; issuing and revoking are next door, in KeysPanel.
//
// Two refusals are the whole reason this screen has an error line rather than
// a toast, and both of them explain themselves:
//
//   - **The last admin.** Removing them, or moving them off admin, comes back
//     409 with a sentence saying which grant it is and what to do instead. A
//     project nobody administers can only be fixed by an operator.
//   - **An address the identity provider does not know.** It comes back 404,
//     because the platform resolves an address to the issuer's `sub` before it
//     writes anything: a grant naming a stranger would be a grant that
//     silently matches nobody.
//
// Neither is a failure of the dashboard's, and neither is guessable from a
// dismissed toast, so both are shown where the form that caused them is.

const props = defineProps<{ project: string; role?: string }>();

const toast = useToast();

const caller = computed(() => callerFor(props.role, props.project));
const mayAdd = computed(() => may("POST /api/v1/projects/{name}/members", caller.value));
const mayChange = computed(() => may("PATCH /api/v1/projects/{name}/members", caller.value));
const mayRemove = computed(() => may("DELETE /api/v1/projects/{name}/members", caller.value));

const { data, error, loading, refresh } = useAsync(() => api.members(props.project));
watch(
  () => props.project,
  () => void refresh(),
);

const members = computed(() => data.value ?? []);
const roleOptions = MEMBER_ROLE_OPTIONS;
const roleSummary = ROLE_SUMMARY;

// What went wrong with the last write, in the API's own words. It is one line
// rather than one per control because only one write is ever in flight.
const writeError = ref("");

const email = ref("");
const newRole = ref("developer");
const adding = ref(false);

async function add() {
  if (!email.value.trim() || adding.value) return;
  adding.value = true;
  writeError.value = "";
  try {
    const member = await api.addMember(props.project, { email: email.value.trim(), role: newRole.value });
    toast.add({
      title: `${memberLabel(member)} is a ${member.role} on ${props.project}`,
      color: "success",
      icon: "i-lucide-user-plus",
    });
    email.value = "";
    await refresh();
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
  } finally {
    adding.value = false;
  }
}

// Changing a role writes on the select's own change: there is one field and
// one value, so a Save button next to it would only be a second click to make
// the same decision.
const changing = ref("");
async function changeRole(member: Member, role: string) {
  if (role === member.role) return;
  changing.value = member.subject;
  writeError.value = "";
  try {
    await api.changeMemberRole(props.project, member.subject, role);
    toast.add({
      title: `${memberLabel(member)} is now a ${role}`,
      color: "success",
      icon: "i-lucide-user-cog",
    });
    await refresh();
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
    // The select is showing the role the write did not make; the reload puts
    // the object's own answer back under it.
    await refresh();
  } finally {
    changing.value = "";
  }
}

const removing = ref<Member | null>(null);
const removed = ref(false);
async function remove() {
  const member = removing.value;
  if (!member || removed.value) return;
  removed.value = true;
  writeError.value = "";
  try {
    await api.removeMember(props.project, member.subject);
    toast.add({ title: `${memberLabel(member)} no longer has a role on ${props.project}`, color: "success", icon: "i-lucide-user-minus" });
    removing.value = null;
    await refresh();
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
    removing.value = null;
  } finally {
    removed.value = false;
  }
}

// A key's grant is removed here like anybody else's, and that takes the role
// off without revoking the credential — which is a key that authenticates and
// can do nothing. The confirmation says so, and says where the other half is.
const removalBlurb = computed(() => {
  const member = removing.value;
  if (!member) return "";
  if (memberKind(member) === "key") {
    return `This takes the ${member.role} role off the key and leaves the credential itself working — it will authenticate and be able to do nothing. Revoking it outright is the CI keys list below.`;
  }
  return `They lose their ${member.role} role on ${props.project} — its builds, its logs and its protected previews go with it. Nothing they built is removed, and they can be added back.`;
});

const readOnlyReason = computed(() => refusal("PATCH /api/v1/projects/{name}/members", caller.value));
</script>

<template>
  <div class="space-y-4 max-w-2xl">
    <div>
      <h2 class="text-sm font-semibold text-highlighted">People</h2>
      <p class="text-xs text-muted mt-1">
        Who may do what on <span class="font-mono">{{ project }}</span
        >. Roles are the platform's, not the git provider's — somebody with a role here needs no access to the cluster.
        The platform's operators hold admin on every project and are not listed.
      </p>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <UAlert
      v-if="writeError"
      color="warning"
      variant="soft"
      icon="i-lucide-info"
      :title="writeError"
      close
      @update:open="writeError = ''"
    />

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full min-w-[34rem] text-sm">
        <tbody>
          <tr v-if="!members.length">
            <td class="px-4 py-8 text-center text-muted">
              {{ loading ? "Loading…" : "Nobody is listed on this project yet." }}
            </td>
          </tr>
          <tr v-for="member in members" :key="member.subject" class="border-b border-muted last:border-0">
            <td class="px-4 py-3">
              <p class="text-highlighted flex items-center gap-2">
                <UIcon
                  v-if="memberKind(member) === 'key'"
                  name="i-lucide-key-round"
                  class="text-dimmed shrink-0"
                  :title="`${member.name} is a CI key, not a person`"
                />
                <span :class="memberKind(member) === 'key' ? 'font-mono' : ''">{{ memberLabel(member) }}</span>
                <UBadge v-if="memberKind(member) === 'key'" color="neutral" variant="subtle" size="sm">CI key</UBadge>
              </p>
              <p
                v-if="memberDetail(member)"
                class="text-xs text-dimmed font-mono truncate max-w-xs"
                :title="member.subject"
              >
                {{ memberDetail(member) }}
              </p>
            </td>
            <td class="px-4 py-3 w-72">
              <USelect
                v-if="mayChange"
                :model-value="member.role"
                :items="roleOptionsFor(member)"
                :loading="changing === member.subject"
                size="sm"
                class="w-full"
                @update:model-value="(role: string) => changeRole(member, role)"
              />
              <span v-else class="inline-flex items-center gap-2">
                <UBadge color="neutral" variant="subtle" size="sm" class="font-mono">{{ member.role }}</UBadge>
                <span class="text-xs text-muted">{{ roleSummary[member.role] }}</span>
              </span>
            </td>
            <td class="px-4 py-3 text-right whitespace-nowrap">
              <UButton
                v-if="mayRemove"
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-user-minus"
                :aria-label="`Remove ${memberLabel(member)}`"
                @click="removing = member"
              />
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <form v-if="mayAdd" class="flex items-end gap-2 flex-wrap" @submit.prevent="add">
      <UFormField label="Add somebody" help="The address they sign in with. The platform resolves it at the identity provider before it writes anything.">
        <UInput v-model="email" type="email" placeholder="anna@example.com" class="w-64" />
      </UFormField>
      <UFormField label="Role">
        <USelect v-model="newRole" :items="roleOptions" class="w-72" />
      </UFormField>
      <UButton type="submit" :disabled="!email.trim()" :loading="adding" icon="i-lucide-user-plus">Add</UButton>
    </form>
    <p v-else-if="readOnlyReason" class="text-xs text-muted">{{ readOnlyReason }}.</p>

    <UModal
      :open="removing !== null"
      :title="`Remove ${removing ? memberLabel(removing) : ''}?`"
      :description="removalBlurb"
      @update:open="(open: boolean) => { if (!open) removing = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="removing = null">Cancel</UButton>
          <UButton color="error" :loading="removed" icon="i-lucide-user-minus" @click="remove">
            Remove from {{ project }}
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
