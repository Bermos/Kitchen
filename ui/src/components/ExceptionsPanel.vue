<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type Exception } from "../lib/api";
import { timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { useAsync } from "../lib/useAsync";

// The exception register: every break-glass grant standing open, and — with
// the historical toggle — everything that ever was. Exceptions are the loud
// half of "never block an emergency deployment": the rules still fire, the
// waiver is named on every decision, and this table is where the standing
// risk is looked in the eye.

const historical = ref(false);
const exceptions = useAsync(() => api.exceptions({ historical: historical.value }));
function toggleHistorical() {
  historical.value = !historical.value;
  void exceptions.refresh();
}

const rows = computed(() => exceptions.data.value ?? []);

// The route wants admin on the exception's project; this screen is the
// operator's, so the table's word decides whether the button exists at all.
const mayResolve = computed(() => may("PATCH /api/v1/exceptions/{name}", callerFor()));

const resolving = ref("");
const resolveError = ref("");

async function resolve(exception: Exception) {
  const reason = window.prompt(
    `Resolving ${exception.name} ends the waiver of ${exception.ruleIDs.join(", ")} for ` +
      `${exception.environment}. Why is it resolved? (recorded in the audit log)`,
  );
  if (!reason || !reason.trim()) return;
  resolving.value = exception.name;
  resolveError.value = "";
  try {
    await api.resolveException(exception.name, reason.trim());
    void exceptions.refresh();
  } catch (cause) {
    resolveError.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    resolving.value = "";
  }
}

function time(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("en-GB");
}

const phaseTone: Record<string, string> = {
  Active: "text-warning",
  Expired: "text-error",
  Resolved: "text-dimmed",
};
</script>

<template>
  <div class="space-y-2">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <p class="text-sm text-highlighted font-medium">Break-glass exceptions</p>
        <p class="text-xs text-muted mt-0.5">
          Every standing waiver: which rules, whose word, until when — and every promotion that went out under it.
          An expired one blocks further promotions until it is resolved or replaced.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <UButton size="xs" color="neutral" :variant="historical ? 'solid' : 'subtle'" @click="toggleHistorical">
          {{ historical ? "Showing history" : "Active only" }}
        </UButton>
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="exceptions.loading.value"
          aria-label="Refresh exceptions"
          @click="exceptions.refresh"
        />
      </div>
    </div>

    <UAlert
      v-if="exceptions.error.value"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="exceptions.error.value"
    />
    <UAlert v-if="resolveError" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="resolveError" />

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default bg-muted">
            <th class="px-3 py-2 font-medium">Exception</th>
            <th class="px-3 py-2 font-medium">Project</th>
            <th class="px-3 py-2 font-medium">Environment</th>
            <th class="px-3 py-2 font-medium">Waives</th>
            <th class="px-3 py-2 font-medium">Approved by</th>
            <th class="px-3 py-2 font-medium">Expires</th>
            <th class="px-3 py-2 font-medium">Phase</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!rows.length">
            <td colspan="8" class="px-3 py-8 text-center text-muted text-sm">
              {{
                exceptions.loading.value
                  ? "Loading…"
                  : historical
                    ? "No exceptions on record."
                    : "No active exceptions — nothing is waived anywhere right now."
              }}
            </td>
          </tr>
          <tr v-for="exception in rows" :key="exception.name" class="border-b border-muted last:border-0 align-top">
            <td class="px-3 py-2 text-xs">
              <span class="font-mono text-highlighted">{{ exception.name }}</span>
              <p class="text-[11px] text-toned break-words max-w-64">{{ exception.reason }}</p>
              <p v-if="exception.incidentRef" class="text-[11px] text-dimmed font-mono">{{ exception.incidentRef }}</p>
            </td>
            <td class="px-3 py-2 text-xs">
              <RouterLink :to="{ name: 'project', params: { name: exception.project } }" class="text-primary hover:underline">
                {{ exception.project }}
              </RouterLink>
            </td>
            <td class="px-3 py-2 text-xs font-mono text-toned break-all">
              {{ exception.environment }}
              <p v-if="exception.release" class="text-[11px] text-dimmed">only {{ exception.release }}</p>
            </td>
            <td class="px-3 py-2 text-xs font-mono text-warning break-all">{{ exception.ruleIDs.join(", ") }}</td>
            <td class="px-3 py-2 text-xs">
              <span class="font-mono text-highlighted break-all">{{ exception.approvedBy }}</span>
              <p class="text-[11px] text-dimmed break-all">asked: {{ exception.requestedBy }}</p>
            </td>
            <td class="px-3 py-2 text-xs text-dimmed font-mono whitespace-nowrap" :title="exception.expiresAt">
              {{ time(exception.expiresAt) }}
              <p class="text-[11px]">{{ timeAgo(exception.expiresAt) }}</p>
            </td>
            <td class="px-3 py-2 text-xs font-mono" :class="phaseTone[exception.phase] ?? 'text-toned'">
              {{ exception.phase }}
              <p v-if="exception.usedBy?.length" class="text-[11px] text-dimmed font-sans">
                used by {{ exception.usedBy.length }} promotion(s)
              </p>
              <p v-if="exception.resolvedBy" class="text-[11px] text-dimmed font-sans break-all">
                by {{ exception.resolvedBy }}
              </p>
            </td>
            <td class="px-3 py-2 text-right whitespace-nowrap">
              <UButton
                v-if="mayResolve && exception.phase !== 'Resolved'"
                size="xs"
                color="neutral"
                variant="subtle"
                :loading="resolving === exception.name"
                title="End this waiver, with a reason on the record"
                @click="resolve(exception)"
              >
                Resolve
              </UButton>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
