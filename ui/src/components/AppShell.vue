<script setup lang="ts">
import { computed } from "vue";
import { useRoute } from "vue-router";
import { api } from "../lib/api";
import { user, signOut } from "../lib/auth";
import { operatorMode } from "../lib/mode";
import { unhealthyConditions, type Tone } from "../lib/status";
import { useAsync, usePoll } from "../lib/useAsync";
import CommandPalette from "./CommandPalette.vue";
import StatusDot from "./StatusDot.vue";

const route = useRoute();

// One inventory fetch feeds the sidebar: the project list, the counts next to
// the nav items, and the preview count on each project row.
const inventory = useAsync(async () => {
  const [projects, environments, builds] = await Promise.all([api.projects(), api.environments(), api.builds()]);
  return { projects, environments, builds };
});
const settings = useAsync(() => api.settings());
usePoll(() => void inventory.refresh(), 30000, () => true);

const projects = computed(() => inventory.data.value?.projects ?? []);

function projectTone(name: string): Tone {
  const project = projects.value.find((p) => p.name === name);
  if (!project?.conditions?.length) return "neutral";
  return unhealthyConditions(project.conditions).length ? "warning" : "success";
}

function previewCount(name: string): number {
  return (inventory.data.value?.environments ?? []).filter((e) => e.project === name && e.type === "preview").length;
}

const nav = computed(() => [
  {
    label: "Overview",
    icon: "i-lucide-layout-dashboard",
    to: "/",
    name: "overview",
    count: inventory.data.value?.projects.length,
  },
  {
    label: "Builds",
    icon: "i-lucide-hammer",
    to: "/builds",
    name: "builds",
    count: inventory.data.value?.builds.length,
  },
  { label: "Observability", icon: "i-lucide-activity", to: "/observability", name: "observability", count: undefined },
  { label: "Connections", icon: "i-lucide-plug", to: "/connections", name: "connections", count: undefined },
  { label: "Settings", icon: "i-lucide-settings-2", to: "/settings", name: "settings", count: undefined },
]);

function navActive(item: { name: string }): boolean {
  if (item.name === "overview") return route.name === "overview";
  if (item.name === "builds") return route.name === "builds" || route.name === "build";
  return route.name === item.name;
}

const activeProject = computed(() => {
  if (route.name === "project") return route.params.name as string;
  return null;
});

const gateway = computed(() => {
  const s = settings.data.value;
  if (!s) return null;
  const programmed = s.conditions?.find((c) => c.type === "GatewayProgrammed");
  return {
    address: s.gatewayAddress || "—",
    healthy: programmed ? programmed.status === "True" : Boolean(s.gatewayAddress),
  };
});

const userMenu = computed(() => [
  [{ label: user.value?.email || user.value?.name || "Signed in", type: "label" as const }],
  [
    {
      label: "Sign out",
      icon: "i-lucide-log-out",
      onSelect: () => {
        signOut();
        window.location.assign("/login");
      },
    },
  ],
]);
</script>

<template>
  <div class="min-h-screen flex">
    <aside class="w-56 shrink-0 border-r border-default bg-muted flex flex-col">
      <RouterLink to="/" class="flex items-center gap-2 px-4 h-14 border-b border-default">
        <img src="/favicon.svg" alt="" class="size-5" />
        <span class="font-semibold text-highlighted">Kitchen</span>
      </RouterLink>

      <nav class="p-2 space-y-0.5">
        <RouterLink
          v-for="item in nav"
          :key="item.name"
          :to="item.to"
          class="flex items-center gap-2.5 px-2.5 py-1.5 rounded-md text-sm hover:bg-elevated hover:text-highlighted"
          :class="navActive(item) ? 'bg-elevated text-highlighted' : 'text-toned'"
        >
          <UIcon :name="item.icon" class="size-4 shrink-0" />
          {{ item.label }}
          <span v-if="item.count !== undefined" class="ml-auto font-mono text-xs text-dimmed">{{ item.count }}</span>
        </RouterLink>
      </nav>

      <div class="px-4 pt-4 pb-1 flex items-center justify-between">
        <span class="text-[11px] font-medium tracking-wider text-dimmed uppercase">Projects</span>
      </div>
      <nav class="px-2 space-y-0.5 overflow-y-auto flex-1">
        <RouterLink
          v-for="project in projects"
          :key="project.name"
          :to="{ name: 'project', params: { name: project.name } }"
          class="flex items-center gap-2.5 px-2.5 py-1.5 rounded-md text-sm hover:bg-elevated hover:text-highlighted"
          :class="activeProject === project.name ? 'bg-elevated text-highlighted' : 'text-toned'"
        >
          <StatusDot :tone="projectTone(project.name)" />
          <span class="truncate">{{ project.name }}</span>
          <span v-if="previewCount(project.name)" class="ml-auto font-mono text-xs text-dimmed">
            {{ previewCount(project.name) }}
          </span>
        </RouterLink>
        <p v-if="inventory.data.value && !projects.length" class="px-2.5 py-1.5 text-xs text-dimmed">
          No projects yet — create one with kubectl for now.
        </p>
      </nav>

      <div v-if="gateway" class="px-4 py-3 border-t border-default text-xs space-y-1.5">
        <div class="flex items-center gap-2">
          <StatusDot :tone="gateway.healthy ? 'success' : 'warning'" />
          <span class="text-muted">Gateway</span>
          <span class="ml-auto font-mono text-toned">{{ gateway.healthy ? "healthy" : "pending" }}</span>
        </div>
        <div class="flex items-center gap-2">
          <span class="text-dimmed pl-3.5 font-mono truncate" :title="gateway.address">{{ gateway.address }}</span>
        </div>
      </div>
    </aside>

    <div class="flex-1 min-w-0 flex flex-col">
      <header class="h-14 shrink-0 border-b border-default flex items-center gap-3 px-6">
        <span class="flex-1" />
        <CommandPalette />
        <span class="flex-1" />
        <UFieldGroup size="sm">
          <UButton
            :color="operatorMode ? 'neutral' : 'primary'"
            :variant="operatorMode ? 'subtle' : 'soft'"
            label="Developer"
            @click="operatorMode = false"
          />
          <UButton
            :color="operatorMode ? 'primary' : 'neutral'"
            :variant="operatorMode ? 'soft' : 'subtle'"
            label="Operator"
            @click="operatorMode = true"
          />
        </UFieldGroup>
        <UDropdownMenu :items="userMenu">
          <UButton color="neutral" variant="ghost" size="sm" icon="i-lucide-circle-user-round">
            {{ user?.name || "Account" }}
          </UButton>
        </UDropdownMenu>
      </header>

      <main class="flex-1 overflow-y-auto">
        <div class="max-w-6xl mx-auto px-6 py-6">
          <slot />
        </div>
      </main>
    </div>
  </div>
</template>
