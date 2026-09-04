<script setup lang="ts">
/**
 * What is wrong, above the table rather than inside it.
 *
 * A failing project used to be one row among four healthy ones, at the same
 * visual weight and in the same scan order, with the only string that would
 * let anybody decide whether to act — the error — truncated at `max-w-sm`.
 * Resolving it took two navigations: the phase badge for the log, the
 * environment screen for the rollback.
 *
 * So a failing or degraded thing leaves the table and comes here, with the
 * three things a triage needs beside each other: the error at its full length,
 * the blast radius as its own sentence, and the way out as a button. The row
 * stays in the table below, dimmed and marked "in band", because the table is
 * the inventory and an inventory with holes in it is worse than one that
 * repeats itself.
 *
 * `lib/attention.ts` derives all of it from the three lists the overview
 * already holds and carries the reasoning for the cap and for what a dismissal
 * means; `docs/UI.md` records both as decisions.
 */
import { computed, ref } from "vue";
import { api } from "../lib/api";
import { ATTENTION_CAP, dismiss, type Incident } from "../lib/attention";
import { timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import ConditionsTable from "./ConditionsTable.vue";
import OperatorOnly from "./OperatorOnly.vue";
import StatusDot from "./StatusDot.vue";

const props = defineProps<{ incidents: Incident[] }>();
const emit = defineEmits<{ (event: "acted"): void }>();

const toast = useToast();

// The cap folds rather than truncates: the count is on the button, so a sixth
// thing being wrong is never something the screen quietly failed to mention.
const showAll = ref(false);
const shown = computed(() => (showAll.value ? props.incidents : props.incidents.slice(0, ATTENTION_CAP)));
const hidden = computed(() => Math.max(0, props.incidents.length - shown.value.length));

// Expanding is per incident and closed by default: the diagnosis is here for
// the one that is being worked on, not for all five at once.
const expanded = ref(new Set<string>());
function toggle(incident: Incident) {
  const next = new Set(expanded.value);
  if (!next.delete(incident.key)) next.add(incident.key);
  expanded.value = next;
}

const acting = ref("");

// The role rides on the incident, from the project the API answered with —
// the same table the API enforces, so what the band offers and what the call
// would allow are one decision. A caller who may not act is offered the way
// in rather than a disabled button (docs/UI.md).
function mayRetry(incident: Incident): boolean {
  return may("POST /api/v1/projects/{name}/builds", callerFor(incident.role, incident.project));
}
function mayRollBack(incident: Incident): boolean {
  return may("PATCH /api/v1/environments/{name}", callerFor(incident.role, incident.project));
}

/**
 * Build the same commit again, from here.
 *
 * The revision is the failed build's own, not the branch's head: the thing
 * being retried is this failure — a dependency that was briefly unresolvable,
 * a registry that refused a push — and retrying "whatever is on the branch
 * now" would be a different build wearing this one's button.
 */
async function retry(incident: Incident) {
  const build = incident.build;
  if (!build) return;
  acting.value = incident.key;
  try {
    const started = await api.rebuild(incident.project, { sha: build.git.sha, branch: build.git.branch });
    toast.add({ title: `Building ${started.name}`, color: "success", icon: "i-lucide-hammer" });
    emit("acted");
  } catch (err) {
    toast.add({
      title: "Could not start the build",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
      icon: "i-lucide-triangle-alert",
    });
  } finally {
    acting.value = "";
  }
}

// Rolling back is not done from here, and that is deliberate: it is the one
// destructive write the dashboard offers, and #181 made it three steps — pick,
// review the diff, verify — precisely because a one-click version reveals
// nothing the release list had not already shown. The band's job is to put
// that flow one click away with the reason for taking it already read.
function rollbackTo(incident: Incident) {
  return { name: "environment", params: { name: incident.environment?.name ?? "" }, query: { rollback: "1" } };
}
</script>

<template>
  <section v-if="incidents.length" class="space-y-2">
    <div class="flex items-center justify-between gap-3 flex-wrap">
      <div class="flex items-center gap-2">
        <StatusDot tone="error" />
        <h2 class="text-sm font-medium text-highlighted">Needs attention</h2>
        <span class="font-mono text-xs text-muted">{{ incidents.length }}</span>
      </div>
      <p class="text-[11px] text-dimmed">newest first · dismissing keeps the row in the table below</p>
    </div>

    <div
      v-for="incident in shown"
      :key="incident.key"
      class="rounded-md border border-default border-l-2 border-l-error overflow-hidden"
    >
      <div class="px-3 py-2.5 flex items-start gap-4 flex-wrap sm:flex-nowrap">
        <div class="min-w-0 sm:w-64 shrink-0">
          <div class="flex items-center gap-2">
            <StatusDot tone="error" />
            <RouterLink
              :to="{ name: 'project', params: { name: incident.project } }"
              class="text-highlighted font-medium hover:underline truncate"
              >{{ incident.project }}</RouterLink
            >
            <UBadge color="neutral" variant="subtle" size="sm">{{ incident.scope }}</UBadge>
          </div>
          <p class="text-xs text-muted font-mono truncate mt-0.5" :title="incident.facts.join(' · ')">
            {{ incident.facts.join(" · ") }}
          </p>
        </div>

        <!-- The error at its full length, which is the whole reason this row
             is not in the table: it is the one string that lets somebody
             decide whether to act, and it is the one the table truncated. -->
        <div class="min-w-0 flex-1">
          <p class="text-sm font-mono text-error break-words">{{ incident.error }}</p>
          <p class="text-xs text-muted mt-1">{{ incident.blastRadius }}</p>
        </div>

        <div class="flex items-center gap-2 shrink-0">
          <span class="text-xs text-muted tabular-nums whitespace-nowrap">{{ timeAgo(incident.since) }}</span>
          <UButton
            v-if="incident.kind === 'build' && mayRetry(incident)"
            size="xs"
            color="neutral"
            variant="solid"
            icon="i-lucide-refresh-cw"
            :loading="acting === incident.key"
            @click="retry(incident)"
          >
            Retry build
          </UButton>
          <UButton
            v-else-if="incident.kind === 'environment' && mayRollBack(incident)"
            size="xs"
            color="neutral"
            variant="solid"
            icon="i-lucide-undo-2"
            :to="rollbackTo(incident)"
          >
            Roll back
          </UButton>
          <UButton
            v-if="incident.build"
            size="xs"
            color="neutral"
            variant="subtle"
            :to="{ name: 'build', params: { name: incident.build.name } }"
          >
            Open build
          </UButton>
          <UButton
            v-else-if="incident.environment"
            size="xs"
            color="neutral"
            variant="subtle"
            :to="{ name: 'environment', params: { name: incident.environment.name } }"
          >
            Open environment
          </UButton>
          <UButton
            v-else
            size="xs"
            color="neutral"
            variant="subtle"
            :to="{ name: 'project', params: { name: incident.project } }"
          >
            Open project
          </UButton>
          <UButton
            size="xs"
            color="neutral"
            variant="ghost"
            :icon="expanded.has(incident.key) ? 'i-lucide-chevron-up' : 'i-lucide-chevron-down'"
            :aria-label="expanded.has(incident.key) ? 'Hide the diagnosis' : 'Show the diagnosis'"
            @click="toggle(incident)"
          />
          <UButton
            size="xs"
            color="neutral"
            variant="ghost"
            icon="i-lucide-x"
            title="Dismiss until this changes — the row stays in the table below"
            aria-label="Dismiss"
            @click="dismiss(incident)"
          />
        </div>
      </div>

      <!-- The diagnosis, without leaving the screen: what the failing step
           said, and — for whoever reads them — the object's conditions. -->
      <div v-if="expanded.has(incident.key)" class="border-t border-default px-3 py-3 space-y-3 bg-muted">
        <div v-if="incident.log.length">
          <h3 class="text-xs font-medium text-muted mb-1">Where it stopped</h3>
          <pre
            class="rounded-md border border-default bg-default px-3 py-2 text-xs font-mono text-toned overflow-x-auto whitespace-pre"
            >{{ incident.log.join("\n") }}</pre
          >
        </div>
        <p v-else class="text-xs text-muted">
          No output was kept for this one — the build's own screen has whatever the log store still holds.
        </p>

        <OperatorOnly>
          <ConditionsTable :conditions="incident.conditions" />
        </OperatorOnly>
      </div>
    </div>

    <UButton
      v-if="hidden || showAll"
      size="xs"
      color="neutral"
      variant="subtle"
      :icon="showAll ? 'i-lucide-chevron-up' : 'i-lucide-chevron-down'"
      @click="showAll = !showAll"
    >
      {{ showAll ? "Show fewer" : `${hidden} more` }}
    </UButton>
  </section>
</template>
