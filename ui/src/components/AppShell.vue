<script setup lang="ts">
import { computed } from "vue";
import { useRoute } from "vue-router";
import { api } from "../lib/api";
import { user, signOut } from "../lib/auth";
import { loadConfig } from "../lib/config";
import { operatorMode } from "../lib/mode";
import { unhealthyConditions, type Tone } from "../lib/status";
import { useAsync, usePoll } from "../lib/useAsync";
import CommandPalette from "./CommandPalette.vue";
import NewProjectModal from "./NewProjectModal.vue";
import StatusDot from "./StatusDot.vue";

const route = useRoute();

// One inventory fetch feeds the sidebar: the project list, the counts next to
// the nav items, and the preview count on each project row.
const inventory = useAsync(async () => {
  const [projects, environments, builds] = await Promise.all([api.projects(), api.environments(), api.builds()]);
  return { projects, environments, builds };
});
// The platform as it is running, for the status bar at the foot of the
// sidebar. It is one request rather than four: cluster, tunnel, build queue
// and gateway all come off /status.
const status = useAsync(() => api.status());
usePoll(() => void inventory.refresh(), 30000, () => true);
usePoll(() => void status.refresh(), 30000, () => true);

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
  { label: "Traffic", icon: "i-lucide-waypoints", to: "/traffic", name: "traffic", count: undefined },
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
  const s = status.data.value;
  if (!s) return null;
  return { address: s.gateway.address || "—", healthy: s.gateway.programmed };
});

// "chef · 8 nodes", the cluster this platform owns. A node count the operator
// may not read comes back as zero with a message, so the count is only shown
// once there is one.
const cluster = computed(() => {
  const c = status.data.value?.cluster;
  if (!c) return null;
  return {
    label: [c.name, c.nodes ? `${c.nodes} node${c.nodes === 1 ? "" : "s"}` : ""].filter(Boolean).join(" · "),
    healthy: c.nodes === 0 || c.readyNodes === c.nodes,
    title: c.message || (c.nodes ? `${c.readyNodes} of ${c.nodes} nodes ready` : ""),
  };
});

// The build queue as the gate sees it: running against the concurrency limit,
// with anything waiting called out.
const builds = computed(() => {
  const b = status.data.value?.builds;
  if (!b) return null;
  return {
    label: `${b.running} of ${b.capacity}`,
    queued: b.queued,
    busy: b.running > 0 || b.queued > 0,
  };
});

// The release the operator was built from — the platform's version, since one
// release publishes the chart and both images. It rides in /config.json, so it
// is there before anyone signs in and costs no extra request.
const platform = useAsync(() => loadConfig());
const version = computed(() => {
  const v = platform.data.value?.version;
  if (!v) return null;
  return v === "dev" ? "dev" : `v${v}`;
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
        <NewProjectModal @created="() => void inventory.refresh()">
          <UButton
            icon="i-lucide-plus"
            color="neutral"
            variant="ghost"
            size="xs"
            aria-label="New project"
            class="-mr-1.5"
          />
        </NewProjectModal>
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
          No projects yet — the + above creates one.
        </p>
      </nav>

      <div class="px-4 py-3 border-t border-default text-xs space-y-1.5">
        <div v-if="cluster" class="flex items-center gap-2" :title="cluster.title">
          <StatusDot :tone="cluster.healthy ? 'success' : 'warning'" />
          <span class="text-toned truncate">{{ cluster.label || "cluster" }}</span>
        </div>
        <template v-if="gateway">
          <div class="flex items-center gap-2">
            <StatusDot :tone="gateway.healthy ? 'success' : 'warning'" />
            <span class="text-muted">Gateway</span>
            <span class="ml-auto font-mono text-toned">{{ gateway.healthy ? "healthy" : "pending" }}</span>
          </div>
          <div class="flex items-center gap-2">
            <span class="text-dimmed pl-3.5 font-mono truncate" :title="gateway.address">{{ gateway.address }}</span>
          </div>
        </template>
        <div v-if="status.data.value?.tunnel.enabled" class="flex items-center gap-2">
          <StatusDot :tone="status.data.value.tunnel.connected ? 'success' : 'warning'" />
          <span class="text-muted">Tunnel</span>
          <span class="ml-auto font-mono text-toned" :title="status.data.value.tunnel.message">
            {{ status.data.value.tunnel.connected ? "connected" : "pending" }}
          </span>
        </div>
        <div v-if="builds" class="flex items-center gap-2">
          <StatusDot :tone="builds.busy ? 'warning' : 'neutral'" :pulse="builds.busy" />
          <span class="text-muted">Builds</span>
          <span
            class="ml-auto font-mono text-toned"
            :title="builds.queued ? `${builds.queued} waiting for a slot` : 'no builds waiting'"
          >
            {{ builds.label }}<template v-if="builds.queued"> · {{ builds.queued }} queued</template>
          </span>
        </div>
        <div v-if="version" class="flex items-center gap-2">
          <span class="text-muted">Kitchen</span>
          <span class="ml-auto font-mono text-dimmed" :title="`Kitchen ${version}`">{{ version }}</span>
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
