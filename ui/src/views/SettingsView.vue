<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api } from "../lib/api";
import { operatorMode } from "../lib/mode";
import { useAsync } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";

// The Kitchen singleton: platform-wide configuration, editable from here —
// which is the reason it is a custom resource and not just Helm values.

const toast = useToast();
const { data: settings, error, loading, refresh } = useAsync(() => api.settings());

const strategy = ref<string>("auto");
const concurrency = ref<number>(2);
const retention = ref<number>(30);

watch(settings, (value) => {
  if (!value) return;
  strategy.value = value.buildStrategy || "auto";
  concurrency.value = value.buildConcurrency ?? 2;
  retention.value = value.logRetentionDays ?? 30;
});

const dirty = computed(() => {
  const s = settings.value;
  if (!s) return false;
  return (
    strategy.value !== (s.buildStrategy || "auto") ||
    concurrency.value !== (s.buildConcurrency ?? 2) ||
    retention.value !== (s.logRetentionDays ?? 30)
  );
});

const saving = ref(false);
async function save() {
  saving.value = true;
  try {
    await api.updateSettings({
      buildStrategy: strategy.value,
      buildConcurrency: concurrency.value,
      logRetentionDays: retention.value,
    });
    toast.add({ title: "Settings saved", color: "success", icon: "i-lucide-check" });
    await refresh();
  } catch (err) {
    toast.add({ title: "Saving failed", description: err instanceof Error ? err.message : String(err), color: "error" });
  } finally {
    saving.value = false;
  }
}

const strategies = [
  { label: "auto — detect the framework", value: "auto" },
  { label: "dockerfile — the project's own", value: "dockerfile" },
  { label: "buildpacks", value: "buildpacks" },
];
</script>

<template>
  <div class="space-y-6 max-w-3xl">
    <div>
      <h1 class="text-xl font-semibold text-highlighted">Settings</h1>
      <p class="text-xs text-muted mt-1">
        The platform's runtime configuration — the <span class="font-mono">Kitchen</span> singleton the operator
        reconciles.
      </p>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <template v-else-if="settings">
      <div class="rounded-md border border-default bg-muted px-5 py-4 grid gap-x-8 gap-y-4 sm:grid-cols-2 text-sm">
        <div>
          <p class="text-xs text-muted mb-0.5">Base domain</p>
          <p class="font-mono text-toned">{{ settings.baseDomain || "—" }}</p>
        </div>
        <div>
          <p class="text-xs text-muted mb-0.5">API</p>
          <p class="font-mono text-toned truncate">{{ settings.apiExternalURL || "—" }}</p>
        </div>
        <div>
          <p class="text-xs text-muted mb-0.5">Identity provider</p>
          <p class="font-mono text-toned">
            {{ settings.authEnabled ? settings.authHost || "enabled" : "disabled" }}
          </p>
        </div>
        <div>
          <p class="text-xs text-muted mb-0.5">Gateway</p>
          <p class="font-mono text-toned">
            {{ settings.gatewayClassName || "—" }}<template v-if="settings.gatewayAddress">
              · {{ settings.gatewayAddress }}</template
            >
          </p>
        </div>
      </div>

      <div class="rounded-md border border-default px-5 py-4 space-y-4">
        <h2 class="text-sm font-medium text-highlighted">Builds and telemetry</h2>
        <UFormField label="Default build strategy" help="Projects can override this per repository.">
          <USelect v-model="strategy" :items="strategies" class="w-72" />
        </UFormField>
        <UFormField label="Build concurrency" help="How many builds run at once, platform-wide.">
          <UInputNumber v-model="concurrency" :min="1" :max="32" class="w-40" />
        </UFormField>
        <UFormField label="Log retention (days)" help="TTL the operator keeps on the ClickHouse telemetry tables.">
          <UInputNumber v-model="retention" :min="1" :max="365" class="w-40" />
        </UFormField>
        <div class="flex justify-end">
          <UButton :disabled="!dirty" :loading="saving" icon="i-lucide-save" @click="save">Save changes</UButton>
        </div>
      </div>

      <div v-if="operatorMode">
        <h2 class="text-sm font-medium text-highlighted mb-2">Platform conditions</h2>
        <ConditionsTable :conditions="settings.conditions" />
      </div>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>
  </div>
</template>
