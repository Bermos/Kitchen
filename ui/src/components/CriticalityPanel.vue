<script setup lang="ts">
import { computed, ref } from "vue";
import { api, CRITICALITIES, type CriticalityFunction } from "../lib/api";
import { useAsync } from "../lib/useAsync";

// The criticality mapping, both directions.
//
// The panel leads with what Kitchen does *not* do, and that is not a
// disclaimer bolted on: deciding what is critical is a board's judgement about
// the institution's functions, and a screen that showed a designation without
// saying whose it is would invite somebody to treat the platform's silence as
// an opinion. The copy is on the screen rather than behind a link for exactly
// that reason.
//
// What the panel does show is the part an institution assembling this by hand
// across four systems gets wrong: the mapping. It is derived from the
// reconciled graph on every request, so there is nothing here to keep current.

const minimum = ref("");
const map = useAsync(() => api.complianceCriticality({ criticality: minimum.value || undefined }));

const filterOptions = [
  { label: "every designation", value: "" },
  ...CRITICALITIES.map((value) => ({ label: `${value} and worse`, value: value as string })),
];

function refilter(value: string) {
  minimum.value = value;
  void map.refresh();
}

const functions = computed<CriticalityFunction[]>(() => map.data.value?.functions ?? []);

/** Which functions are opened out to their resources. */
const expanded = ref<Set<string>>(new Set());
function toggle(project: string) {
  const open = new Set(expanded.value);
  if (open.has(project)) open.delete(project);
  else open.add(project);
  expanded.value = open;
}

function tone(criticality: string): string {
  switch (criticality) {
    case "critical":
      return "text-error";
    case "important":
      return "text-warning";
    case "nonCritical":
      return "text-toned";
    default:
      return "text-dimmed";
  }
}

/** The designation as one line, absences included — "undesignated" is a word
 *  here for the same reason it is one in the API's answer. */
function designation(criticality: string, rto?: string, rpo?: string): string {
  const parts = [criticality];
  if (rto) parts.push(`RTO ${rto}`);
  if (rpo) parts.push(`RPO ${rpo}`);
  return parts.join(" · ");
}

function inheritedNote(inherited?: string[]): string {
  return inherited?.length ? `inherited: ${inherited.join(", ")}` : "";
}

// The reverse query. It is asked rather than polled: "what breaks if this is
// unavailable" is a question somebody types during an incident, and a table
// that answered it unprompted would be a table of nothing most days.
const subject = ref("");
const subjectKind = ref<"provider" | "connection">("provider");
const dependents = ref<Awaited<ReturnType<typeof api.complianceDependents>> | null>(null);
const asking = ref(false);
const askError = ref("");

async function ask() {
  const name = subject.value.trim();
  if (!name) return;
  asking.value = true;
  askError.value = "";
  try {
    dependents.value = await api.complianceDependents(
      subjectKind.value === "provider" ? { provider: name } : { connection: name },
    );
  } catch (cause) {
    dependents.value = null;
    askError.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    asking.value = false;
  }
}

const kindOptions = [
  { label: "third party", value: "provider" },
  { label: "connection", value: "connection" },
];
</script>

<template>
  <div class="space-y-3">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div class="max-w-3xl">
        <p class="text-sm text-highlighted font-medium">Criticality and disruption tolerance</p>
        <p class="text-xs text-muted mt-0.5">
          Everything supporting each designated function: its environments and what they run, the resources
          they hold, the connections behind those, and the third parties behind those. Derived from the
          platform's own graph on every request — there is nothing here anybody has to keep current.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <USelect
          :model-value="minimum"
          :items="filterOptions"
          size="xs"
          class="w-52"
          @update:model-value="refilter"
        />
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="map.loading.value"
          aria-label="Refresh the criticality map"
          @click="map.refresh"
        />
      </div>
    </div>

    <!-- The out-of-scope boundary, on the screen that carries the fields
         rather than only in the documentation. -->
    <UAlert color="neutral" variant="subtle" icon="i-lucide-scale">
      <template #description>
        <span class="text-highlighted font-medium">Kitchen does not decide what is critical.</span>
        Which of an institution's functions are critical or important, and how long each may be disrupted
        (RTO) or how much of its data may be lost (RPO), are the institution's own decisions — a board's, not
        a platform's. Kitchen carries the designation once somebody has made it, maps it onto the resources
        that actually serve the function, and holds the estate to the tolerances. It never designates
        anything itself, never defaults an absent designation to anything, and never refuses a deployment
        because of one.
      </template>
    </UAlert>

    <UAlert
      v-if="map.error.value"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="map.error.value"
    />

    <div v-else class="rounded-md border border-default overflow-x-auto">
      <table class="w-full text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default bg-muted">
            <th class="px-3 py-2 font-medium">Function</th>
            <th class="px-3 py-2 font-medium">Designation</th>
            <th class="px-3 py-2 font-medium">Environments</th>
            <th class="px-3 py-2 font-medium">Third parties</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!functions.length">
            <td colspan="4" class="px-3 py-8 text-center text-muted text-sm">
              {{ map.loading.value ? "Loading…" : "Nothing has been designated." }}
            </td>
          </tr>
          <template v-for="fn in functions" :key="fn.project">
            <tr class="border-b border-muted last:border-0 align-top hover:bg-elevated/40">
              <td class="px-3 py-2 text-xs">
                <button class="text-primary hover:underline font-mono" @click="toggle(fn.project)">
                  {{ fn.project }}
                </button>
              </td>
              <td class="px-3 py-2 text-xs font-mono" :class="tone(fn.criticality)">
                {{ designation(fn.criticality, fn.rto, fn.rpo) }}
              </td>
              <td class="px-3 py-2 text-xs text-toned">{{ fn.environments.length }}</td>
              <td class="px-3 py-2 text-xs text-toned break-all">
                {{ fn.thirdParties.join(", ") || "—" }}
              </td>
            </tr>
            <tr v-if="expanded.has(fn.project)" class="border-b border-muted last:border-0">
              <td colspan="4" class="px-3 py-3 bg-elevated/30 space-y-3">
                <div>
                  <p class="text-xs text-muted mb-1">Environments</p>
                  <p v-for="env in fn.environments" :key="env.name" class="text-xs">
                    <span class="font-mono text-highlighted">{{ env.name }}</span>
                    <span class="text-dimmed"> · {{ env.type }} · </span>
                    <span class="font-mono" :class="tone(env.criticality)">
                      {{ designation(env.criticality, env.rto, env.rpo) }}
                    </span>
                    <span v-if="inheritedNote(env.inherited)" class="text-dimmed">
                      ({{ inheritedNote(env.inherited) }})
                    </span>
                    <span v-if="env.release" class="text-toned"> · {{ env.release }}</span>
                    <span v-if="env.domains?.length" class="text-toned"> · {{ env.domains.join(", ") }}</span>
                  </p>
                </div>
                <div v-if="fn.claims.length">
                  <p class="text-xs text-muted mb-1">Resources</p>
                  <p v-for="claim in fn.claims" :key="claim.name" class="text-xs">
                    <span class="font-mono text-highlighted">{{ claim.name }}</span>
                    <span class="text-dimmed"> · {{ claim.type }} · {{ claim.provider || "—" }}</span>
                    <span class="text-toned"> · {{ claim.dataClass }} · {{ claim.residency }}</span>
                  </p>
                </div>
                <div v-if="fn.connections.length">
                  <p class="text-xs text-muted mb-1">Connections</p>
                  <p v-for="conn in fn.connections" :key="conn.name" class="text-xs">
                    <span class="font-mono text-highlighted">{{ conn.name }}</span>
                    <span class="text-dimmed"> · {{ conn.provider || "—" }}</span>
                    <span class="text-toned"> · {{ conn.usedFor.join(", ") }}</span>
                  </p>
                </div>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>

    <p v-if="map.data.value" class="text-xs text-dimmed">
      <template v-if="map.data.value.undesignated">
        {{ map.data.value.undesignated }} project(s) carry no designation at all — nobody has looked, which
        is a different state from designated non-critical.
      </template>
      {{ map.data.value.depth }}
    </p>

    <!-- The reverse query: what breaks if one third party is unavailable. -->
    <div class="rounded-md border border-default p-4 space-y-3">
      <div>
        <p class="text-sm text-highlighted font-medium">What breaks if this is unavailable?</p>
        <p class="text-xs text-muted mt-0.5">
          The same map walked backwards: every environment that depends on one third party, or on one
          connection, worst designation first — with the tightest recovery objective among them, which is how
          long it may be gone before the first tolerance the institution declared is breached.
        </p>
      </div>
      <form class="flex items-end gap-2 flex-wrap" @submit.prevent="ask">
        <USelect v-model="subjectKind" :items="kindOptions" size="sm" class="w-40" />
        <UInput
          v-model="subject"
          size="sm"
          placeholder="neon"
          class="w-64 font-mono"
          aria-label="The third party or connection to ask about"
        />
        <UButton type="submit" size="sm" :loading="asking" :disabled="!subject.trim()" icon="i-lucide-search">
          Ask
        </UButton>
      </form>

      <UAlert v-if="askError" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="askError" />

      <template v-else-if="dependents">
        <p v-if="!dependents.affected.length" class="text-xs text-muted">
          Nothing depends on {{ dependents.subject.name }}.
        </p>
        <template v-else>
          <p class="text-xs text-toned">
            <span class="font-medium">{{ dependents.affected.length }}</span> environment(s) affected.
            <template v-if="dependents.tightestRTO">
              Tightest recovery objective among them:
              <span class="font-mono text-error">{{ dependents.tightestRTO }}</span
              >.
            </template>
            <template v-if="dependents.subject.connections?.length">
              Through {{ dependents.subject.connections.join(", ") }}.
            </template>
          </p>
          <div class="rounded-md border border-default overflow-x-auto">
            <table class="w-full text-xs">
              <thead>
                <tr class="text-left text-muted border-b border-default bg-muted">
                  <th class="px-3 py-2 font-medium">Project</th>
                  <th class="px-3 py-2 font-medium">Environment</th>
                  <th class="px-3 py-2 font-medium">Designation</th>
                  <th class="px-3 py-2 font-medium">Through</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="affected in dependents.affected"
                  :key="`${affected.project}/${affected.environment}`"
                  class="border-b border-muted last:border-0"
                >
                  <td class="px-3 py-1.5 font-mono text-toned">{{ affected.project }}</td>
                  <td class="px-3 py-1.5 font-mono text-highlighted">{{ affected.environment }}</td>
                  <td class="px-3 py-1.5 font-mono" :class="tone(affected.criticality)">
                    {{ designation(affected.criticality, affected.rto, affected.rpo) }}
                  </td>
                  <td class="px-3 py-1.5 text-toned">{{ affected.through.join(", ") }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p class="text-xs text-dimmed">{{ dependents.depth }}</p>
        </template>
      </template>
    </div>
  </div>
</template>
