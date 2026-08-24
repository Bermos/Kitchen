<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { api } from "../lib/api";
import { user, signOut } from "../lib/auth";
import { loadConfig, platformVersion } from "../lib/config";
import { callerFor, forgetMe, meError } from "../lib/me";
import { canSwitchMode, operatorMode } from "../lib/mode";
import { may } from "../lib/policy";
import { unhealthyConditions, type Tone } from "../lib/status";
import { useAsync, usePoll } from "../lib/useAsync";
import CommandPalette from "./CommandPalette.vue";
import NewProjectModal from "./NewProjectModal.vue";
import StatusDot from "./StatusDot.vue";

const route = useRoute();

// Below `lg` the sidebar is an off-canvas drawer rather than a column: on a
// phone it would otherwise eat two thirds of the viewport. It stays one
// element in the DOM either way — the media query alone decides whether it is
// in the flow or slid over the page — so the project list keeps its scroll
// position across the change.
const sidebarOpen = ref(false);
const wide = ref(true);
let media: MediaQueryList | null = null;
function onMedia(event: MediaQueryListEvent | MediaQueryList) {
  wide.value = event.matches;
  if (event.matches) sidebarOpen.value = false;
}
function onKeydown(event: KeyboardEvent) {
  if (event.key === "Escape") sidebarOpen.value = false;
}
onMounted(() => {
  media = window.matchMedia("(min-width: 1024px)");
  onMedia(media);
  media.addEventListener("change", onMedia);
  window.addEventListener("keydown", onKeydown);
});
onUnmounted(() => {
  media?.removeEventListener("change", onMedia);
  window.removeEventListener("keydown", onKeydown);
});
// Navigating is the drawer's other close button: every link in it leads
// somewhere, and none of them should leave it covering the page it opened.
watch(() => route.fullPath, () => (sidebarOpen.value = false));
// A drawer that is off the screen is out of the page as far as the keyboard
// and a screen reader are concerned. `undefined` rather than `false` because
// `inert` is not one of the attributes Vue removes when bound to false.
const sidebarHidden = computed(() => (!wide.value && !sidebarOpen.value) || undefined);

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

// The navigation is the role's, not the mode's. Everything down to Traces is
// filtered server-side to the caller's own projects, so it is everybody's
// screen with everybody's answer in it; Connections and Settings are the
// operator's outright, and a member gets no entry rather than an entry that
// leads to a refusal. Each one names the API route it stands for, so this and
// the route guard are the same decision made twice from the same table.
const nav = computed(() =>
  [
    {
      label: "Overview",
      icon: "i-lucide-layout-dashboard",
      to: "/",
      name: "overview",
      count: inventory.data.value?.projects.length,
      shown: true,
    },
    {
      label: "Builds",
      icon: "i-lucide-hammer",
      to: "/builds",
      name: "builds",
      count: inventory.data.value?.builds.length,
      shown: true,
    },
    {
      label: "Observability",
      icon: "i-lucide-activity",
      to: "/observability",
      name: "observability",
      count: undefined,
      shown: true,
    },
    { label: "Traffic", icon: "i-lucide-waypoints", to: "/traffic", name: "traffic", count: undefined, shown: true },
    { label: "Traces", icon: "i-lucide-git-fork", to: "/traces", name: "traces", count: undefined, shown: true },
    {
      label: "Connections",
      icon: "i-lucide-plug",
      to: "/connections",
      name: "connections",
      count: undefined,
      shown: may("GET /api/v1/connections/{name}", callerFor()),
    },
    {
      label: "Settings",
      icon: "i-lucide-settings-2",
      to: "/settings",
      name: "settings",
      count: undefined,
      shown: may("GET /api/v1/settings", callerFor()),
    },
  ].filter((item) => item.shown),
);

function navActive(item: { name: string }): boolean {
  if (item.name === "overview") return route.name === "overview";
  if (item.name === "builds") return route.name === "builds" || route.name === "build";
  return route.name === item.name;
}

// The operator's section: the platform seen across every project, which is a
// different question from any of the screens above and a differently
// authorized one. It is shown in operator mode alone — and operator mode is
// now something only an operator can be in, so a member never sees it and an
// operator who has switched to the developer's view does not either.
//
// The routes still exist for an operator in both modes: a finding's evidence
// link is a link somebody pastes, and it should land where it says it does.
//
// The paths are the ones the API emits as evidence, and they are load-bearing:
// `internal/signals/evidence.go` names them.
const platformNav = [
  { label: "Overview", icon: "i-lucide-gauge", to: "/platform", name: "platform" },
  { label: "Nodes", icon: "i-lucide-server", to: "/platform/nodes", name: "platform-nodes" },
  { label: "Workloads", icon: "i-lucide-boxes", to: "/platform/workloads", name: "platform-workloads" },
  { label: "Edge", icon: "i-lucide-globe", to: "/platform/edge", name: "platform-edge" },
  { label: "Storage", icon: "i-lucide-hard-drive", to: "/platform/storage", name: "platform-storage" },
  { label: "Events", icon: "i-lucide-list", to: "/platform/events", name: "platform-events" },
  { label: "Backup", icon: "i-lucide-archive", to: "/platform/backup", name: "platform-backup" },
];

const activeProject = computed(() => {
  if (route.name === "project") return route.params.name as string;
  return null;
});

// The gateway is the operator's half of /status: absent, not zeroed, for an
// account that may not read it — so there is simply no tile.
const gateway = computed(() => {
  const s = status.data.value;
  if (!s?.gateway) return null;
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
//
// It is read off `platformVersion` rather than the loaded config because it is
// the one field of that config that can change under an open page: a platform
// upgrade replaces the operator serving it, and the settings page re-reads
// /config.json while the upgrade lands. The number here moves with it instead
// of staying on the old release until somebody reloads.
void loadConfig();
const version = computed(() => {
  const v = platformVersion.value;
  if (!v) return null;
  return v === "dev" ? "dev" : `v${v}`;
});

const userMenu = computed(() => [
  [{ label: user.value?.email || user.value?.name || "Signed in", type: "label" as const }],
  [
    {
      label: "Sign out",
      icon: "i-lucide-log-out",
      onSelect: async () => {
        // Waiting lets the refresh token be revoked at the issuer before the
        // page goes away; a revocation that fails still leaves nothing behind
        // in this browser.
        forgetMe();
        await signOut();
        window.location.assign("/login");
      },
    },
  ],
]);
</script>

<template>
  <div class="min-h-screen flex">
    <!-- The drawer's backdrop, and the largest possible target for closing it. -->
    <div
      v-if="sidebarOpen"
      class="fixed inset-0 z-40 bg-black/60 lg:hidden"
      aria-hidden="true"
      @click="sidebarOpen = false"
    />

    <aside
      id="sidebar"
      :inert="sidebarHidden"
      class="w-56 shrink-0 border-r border-default bg-muted flex flex-col fixed inset-y-0 left-0 z-50 transition-transform lg:static lg:translate-x-0"
      :class="sidebarOpen ? 'translate-x-0' : '-translate-x-full'"
    >
      <div class="flex items-center h-14 border-b border-default">
        <RouterLink to="/" class="flex items-center gap-2 px-4 flex-1 min-w-0">
          <img src="/favicon.svg" alt="" class="size-5" />
          <span class="font-semibold text-highlighted">Kitchen</span>
        </RouterLink>
        <UButton
          icon="i-lucide-x"
          color="neutral"
          variant="ghost"
          size="sm"
          class="mr-2 lg:hidden"
          aria-label="Close navigation"
          @click="sidebarOpen = false"
        />
      </div>

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

      <!-- Operator mode's own section. Everything platform-scoped lives behind
           one prefix and nothing project-scoped does. -->
      <template v-if="operatorMode">
        <div class="px-4 pt-4 pb-1">
          <span class="text-[11px] font-medium tracking-wider text-dimmed uppercase">Platform</span>
        </div>
        <nav class="px-2 space-y-0.5">
          <RouterLink
            v-for="item in platformNav"
            :key="item.name"
            :to="item.to"
            class="flex items-center gap-2.5 px-2.5 py-1.5 rounded-md text-sm hover:bg-elevated hover:text-highlighted"
            :class="route.name === item.name ? 'bg-elevated text-highlighted' : 'text-toned'"
          >
            <UIcon :name="item.icon" class="size-4 shrink-0" />
            {{ item.label }}
          </RouterLink>
        </nav>
      </template>

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
      <nav class="px-2 space-y-0.5 overflow-y-auto flex-1 min-h-0">
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
        <div v-if="status.data.value?.tunnel?.enabled" class="flex items-center gap-2">
          <StatusDot :tone="status.data.value?.tunnel?.connected ? 'success' : 'warning'" />
          <span class="text-muted">Tunnel</span>
          <span class="ml-auto font-mono text-toned" :title="status.data.value?.tunnel?.message">
            {{ status.data.value?.tunnel?.connected ? "connected" : "pending" }}
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
      <header class="h-14 shrink-0 border-b border-default flex items-center gap-2 sm:gap-3 px-3 sm:px-6">
        <UButton
          icon="i-lucide-menu"
          color="neutral"
          variant="ghost"
          size="sm"
          class="lg:hidden"
          aria-label="Open navigation"
          aria-controls="sidebar"
          :aria-expanded="sidebarOpen"
          @click="sidebarOpen = true"
        />
        <span class="flex-1" />
        <CommandPalette />
        <span class="flex-1" />
        <!-- Both labels collapse to their icons on a phone: the pair is the
             widest thing in the header and the least in need of words.

             The switch is an operator's own choice to look at the platform the
             way a developer does. A member has nothing on the other side of it,
             so they get no switch rather than a switch that leads to panels
             they may not fill. -->
        <UFieldGroup v-if="canSwitchMode" size="sm">
          <UButton
            :color="operatorMode ? 'neutral' : 'primary'"
            :variant="operatorMode ? 'subtle' : 'soft'"
            icon="i-lucide-code"
            title="Developer"
            aria-label="Developer view"
            @click="operatorMode = false"
          >
            <span class="hidden sm:inline">Developer</span>
          </UButton>
          <UButton
            :color="operatorMode ? 'primary' : 'neutral'"
            :variant="operatorMode ? 'soft' : 'subtle'"
            icon="i-lucide-server-cog"
            title="Operator"
            aria-label="Operator view"
            @click="operatorMode = true"
          >
            <span class="hidden sm:inline">Operator</span>
          </UButton>
        </UFieldGroup>
        <UDropdownMenu :items="userMenu">
          <UButton
            color="neutral"
            variant="ghost"
            size="sm"
            icon="i-lucide-circle-user-round"
            :aria-label="user?.name || 'Account'"
          >
            <span class="hidden sm:inline">{{ user?.name || "Account" }}</span>
          </UButton>
        </UDropdownMenu>
      </header>

      <main class="flex-1 overflow-y-auto">
        <div class="max-w-6xl mx-auto px-4 sm:px-6 py-5 sm:py-6 space-y-5">
          <!-- What the dashboard renders is decided by the role /me answers
               with, so a /me that never answered is worth saying out loud:
               without it every screen is the narrowest one, and a missing
               Settings entry would otherwise look like a decision somebody
               made rather than a request that failed. -->
          <UAlert
            v-if="meError"
            color="warning"
            variant="soft"
            icon="i-lucide-user-x"
            title="The platform could not say who you are signed in as"
            :description="`${meError} — until it can, only what every account may see is shown.`"
          />
          <slot />
        </div>
      </main>
    </div>
  </div>
</template>
