<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type Member } from "../lib/api";
import { callerFor } from "../lib/me";
import { may, refusal } from "../lib/policy";
import { useAsync } from "../lib/useAsync";

// A project's people, on the project: who holds which role, adding somebody
// by the address they sign in with, moving them to another role, and taking
// them off. It is the rest of the sentence self-service starts — a project's
// creator is its admin the moment it exists, and this is how they hand it to
// anybody else without going through the platform's owner.
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

// Weakest first, the way the roles are ordered everywhere else, each with the
// sentence docs/AUTH.md describes it in — the field is a decision about what
// somebody may do, not a word to pick off a list.
const roleOptions = [
  { label: "viewer — reads status, builds, logs; may open a protected preview", value: "viewer" },
  { label: "developer — builds, redeploys, rollbacks, env vars, domains", value: "developer" },
  { label: "admin — everything a developer may, plus membership and settings", value: "admin" },
];
const roleSummary: Record<string, string> = {
  viewer: "reads, no writes",
  developer: "the day job",
  admin: "membership and settings too",
};

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
      title: `${member.email || member.subject} is a ${member.role} on ${props.project}`,
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
      title: `${describe(member)} is now a ${role}`,
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
    toast.add({ title: `${describe(member)} no longer has a role on ${props.project}`, color: "success", icon: "i-lucide-user-minus" });
    removing.value = null;
    await refresh();
  } catch (err) {
    writeError.value = err instanceof Error ? err.message : String(err);
    removing.value = null;
  } finally {
    removed.value = false;
  }
}

/** A member as a person rather than an identifier: the address where there is
 * one, and the subject where there is not — a machine account, or a grant
 * written by hand against an address. */
function describe(member: Member): string {
  return member.email || member.subject;
}

/** Whether the subject is worth showing next to the address. It is the thing
 * every write addresses a member by, so it is never hidden — only folded away
 * when it is the only name there is. */
function subjectOf(member: Member): string {
  return member.email ? member.subject : "";
}

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
              <p class="text-highlighted">{{ describe(member) }}</p>
              <p v-if="subjectOf(member)" class="text-xs text-dimmed font-mono truncate max-w-xs" :title="member.subject">
                {{ subjectOf(member) }}
              </p>
            </td>
            <td class="px-4 py-3 w-72">
              <USelect
                v-if="mayChange"
                :model-value="member.role"
                :items="roleOptions"
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
                :aria-label="`Remove ${describe(member)}`"
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
      :title="`Remove ${removing ? describe(removing) : ''}?`"
      :description="`They lose their ${removing?.role} role on ${project} — its builds, its logs and its protected previews go with it. Nothing they built is removed, and they can be added back.`"
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
