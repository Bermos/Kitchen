<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api } from "../lib/api";
import { user, signOut } from "../lib/auth";
import { loadConfig, platformVersion } from "../lib/config";
import { callerFor, forgetMe, meError } from "../lib/me";
import { may } from "../lib/policy";
import { completedBy, SCOPES, type Scope } from "../routes";
import { unhealthyConditions, type Tone } from "../lib/status";
import { useAsync, usePoll } from "../lib/useAsync";
import CommandPalette from "./CommandPalette.vue";
import NewProjectModal from "./NewProjectModal.vue";
import StatusDot from "./StatusDot.vue";

const route = useRoute();
const router = useRouter();

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
//
// The shell's two pollers are outside the screen's freshness (`screen: false`)
// on both halves: the sidebar is navigation rather than something being read,
// so it neither ages the screen the reader is looking at nor stops when they
// pause it. See docs/UI.md, "The freshness control".
const inventory = useAsync(
  async () => {
    const [projects, environments, builds] = await Promise.all([api.projects(), api.environments(), api.builds()]);
    return { projects, environments, builds };
  },
  { screen: false },
);
// The platform as it is running, for the status bar at the foot of the
// sidebar. It is one request rather than four: cluster, tunnel, build queue
// and gateway all come off /status.
const status = useAsync(() => api.status(), { screen: false });
usePoll(() => void inventory.refresh(), 30000, () => true, { screen: false });
usePoll(() => void status.refresh(), 30000, () => true, { screen: false });

const projects = computed(() => inventory.data.value?.projects ?? []);

function projectTone(name: string): Tone {
  const project = projects.value.find((p) => p.name === name);
  if (!project?.conditions?.length) return "neutral";
  return unhealthyConditions(project.conditions).length ? "warning" : "success";
}

function previewCount(name: string): number {
  return (inventory.data.value?.environments ?? []).filter((e) => e.project === name && e.type === "preview").length;
}

/**
 * Which scope the shell is in, and what the sidebar is therefore a list of.
 *
 * The dashboard used to render one flat list of every audience's screens, so
 * the operator's two inventories were items six and seven of the developer's
 * navigation and every cross-project developer screen began with a question
 * the address could not answer (#469). Now the scope is in the address, the
 * switcher says which one you are in, and the sidebar is that scope's own.
 *
 * A scope nobody may open is not offered — the same rule every affordance here
 * follows, asked of the same table the route guard asks. Compliance is gated
 * on the audit log rather than on the compliance posture, because four of the
 * five routes behind it are answered for anybody who can see a project.
 */
const scope = computed<Scope>(() => (route.meta.scope as Scope | undefined) ?? "fleet");

/** The project the address names, if it names one. The two legacy addresses
 * in this scope — `/builds/:name`, `/environments/:name` — name an object
 * rather than a project, so the path is asked as well as the parameter. */
const activeProject = computed<string | null>(() => {
  if (!route.path.startsWith("/projects/")) return null;
  const name = route.params.name;
  return typeof name === "string" && name ? name : null;
});

const scopeLabel = computed(() => SCOPES.find((definition) => definition.id === scope.value)?.label ?? "Fleet");

const scopes = computed(() =>
  SCOPES.filter((definition) => !definition.requires || may(definition.requires, callerFor())).map((definition) => ({
    ...definition,
    // Picking Project with a project already open stays on it rather than
    // asking again; picking it without one is the picker.
    to: definition.id === "project" && activeProject.value ? `/projects/${activeProject.value}` : definition.root,
    active: scope.value === definition.id,
  })),
);

/** The environments of the project in the address, for the Project scope's
 * own navigation: `/projects/:name/environments/:env` is otherwise reachable
 * only from a table inside a screen. */
const projectEnvironments = computed(() => {
  const project = activeProject.value;
  if (!project) return [];
  return (inventory.data.value?.environments ?? []).filter((environment) => environment.project === project);
});

interface NavItem {
  label: string;
  icon: string;
  to: string | { name: string; params?: Record<string, string> };
  name: string;
  count?: number;
}

/**
 * The sidebar, per scope.
 *
 * Fleet is the only one that spans projects and it holds the questions that
 * genuinely do — what is wrong, what shipped. (`/alerts` is the third and has
 * no route behind it yet; #471 is what puts something there, and an entry
 * leading nowhere is worse than none.) Everything a developer asks about one
 * project is in the Project scope, addressed by that project.
 */
const nav = computed<NavItem[]>(() => {
  const project = activeProject.value;
  switch (scope.value) {
    case "fleet":
      return [
        {
          label: "Overview",
          icon: "i-lucide-layout-dashboard",
          to: "/",
          name: "overview",
          count: inventory.data.value?.projects.length,
        },
        {
          label: "Deploys",
          icon: "i-lucide-rocket",
          to: "/deploys",
          name: "deploys",
          count: inventory.data.value?.builds.length,
        },
      ];
    case "project":
      if (!project) return [];
      return [
        {
          label: "Overview",
          icon: "i-lucide-layout-dashboard",
          to: { name: "project", params: { name: project } },
          name: "project",
        },
        {
          label: "Deploys",
          icon: "i-lucide-rocket",
          to: { name: "project-deploys", params: { name: project } },
          name: "project-deploys",
        },
        {
          label: "Observability",
          icon: "i-lucide-activity",
          to: { name: "project-observability", params: { name: project } },
          name: "project-observability",
        },
      ];
    case "platform":
      return [
        { label: "Overview", icon: "i-lucide-gauge", to: "/platform", name: "platform" },
        { label: "Nodes", icon: "i-lucide-server", to: "/platform/nodes", name: "platform-nodes" },
        { label: "Workloads", icon: "i-lucide-boxes", to: "/platform/workloads", name: "platform-workloads" },
        { label: "Edge", icon: "i-lucide-globe", to: "/platform/edge", name: "platform-edge" },
        { label: "Addons", icon: "i-lucide-puzzle", to: "/platform/addons", name: "platform-addons" },
        // Both inventories of storage: the volumes projects claimed, and the
        // storage somebody wrote so a project could mount something older
        // than the cluster. They were two screens, one of them in the
        // developer's navigation.
        { label: "Storage", icon: "i-lucide-hard-drive", to: "/platform/storage", name: "platform-storage" },
        { label: "Events", icon: "i-lucide-list", to: "/platform/events", name: "platform-events" },
        // Written once by whoever administers the installation and pointed at
        // by projects afterwards, which is the operator's standing exactly.
        { label: "Connections", icon: "i-lucide-plug", to: "/platform/connections", name: "platform-connections" },
        { label: "Backup", icon: "i-lucide-archive", to: "/platform/backup", name: "platform-backup" },
        { label: "Settings", icon: "i-lucide-settings-2", to: "/platform/settings", name: "platform-settings" },
      ];
    case "compliance":
      return [{ label: "Audit", icon: "i-lucide-shield-check", to: "/compliance/audit", name: "compliance-audit" }];
  }
  return [];
});

function navActive(item: NavItem): boolean {
  // A build opened from the deploy list is still the deploy list as far as
  // the sidebar is concerned; so is an environment opened from a project.
  if (item.name === "deploys") return route.name === "deploys" || route.name === "build";
  if (item.name === "project-deploys") return route.name === "project-deploys" || route.name === "project-build";
  return route.name === item.name;
}

/**
 * A screen that needs a project and was opened without one.
 *
 * `/observability` is not a screen: it is a question about a project nobody
 * named. So the address is kept exactly as it was asked for, the fleet
 * dashboard renders underneath, and the picker completes the sentence — which
 * is what makes a pasted link say what its author meant rather than resolving
 * to whichever project a dropdown happened to be on.
 */
const picker = computed(() => route.meta.picker);
const pickerOpen = ref(false);
watch(
  () => route.fullPath,
  () => (pickerOpen.value = Boolean(route.meta.picker)),
  { immediate: true },
);
function chooseProject(name: string) {
  const target = route.meta.picker;
  if (!target) return;
  pickerOpen.value = false;
  void router.replace(completedBy(target, route, name));
}

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
    // The one thing this menu used to be missing, and the only route to it:
    // changing a password, or ending a session somebody else is holding, has
    // no other screen and never had one (issue #207).
    { label: "Account", icon: "i-lucide-user-round", to: "/account" },
  ],
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

      <div class="px-4 pt-3 pb-1">
        <span class="text-[11px] font-medium tracking-wider text-dimmed uppercase">{{ scopeLabel }}</span>
      </div>
      <nav class="px-2 pb-2 space-y-0.5">
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

      <!-- The Project scope's own list: the environments of the project in
           the address. Without it `/projects/:name/environments/:env` is
           reachable only from a table inside a screen. -->
      <template v-if="scope === 'project' && activeProject">
        <div class="px-4 pt-4 pb-1">
          <span class="text-[11px] font-medium tracking-wider text-dimmed uppercase">Environments</span>
        </div>
        <nav class="px-2 space-y-0.5">
          <RouterLink
            v-for="environment in projectEnvironments"
            :key="environment.name"
            :to="{ name: 'project-environment', params: { name: activeProject, env: environment.name } }"
            class="flex items-center gap-2.5 px-2.5 py-1.5 rounded-md text-sm hover:bg-elevated hover:text-highlighted"
            :class="route.params.env === environment.name ? 'bg-elevated text-highlighted' : 'text-toned'"
          >
            <StatusDot :tone="environment.url ? 'success' : 'neutral'" />
            <span class="truncate">{{ environment.name }}</span>
          </RouterLink>
          <p v-if="!projectEnvironments.length" class="px-2.5 py-1.5 text-xs text-dimmed">
            Nothing deployed yet.
          </p>
        </nav>
      </template>

      <!-- The projects, which are how the Project scope is entered. They are
           in the sidebar in every scope: an operator reading the platform's
           events still gets there from a project name. -->
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
        <!-- Which of the four scopes this address is in, and the way into
             the other three. It is the one thing in this header that says
             what you are looking at: the mode toggle that used to sit here
             did the structure's job while being invisible in the URL, which
             is why it is gone and this is here (#469). -->
        <nav class="flex items-center gap-0.5 rounded-md border border-default p-0.5" aria-label="Scope">
          <RouterLink
            v-for="item in scopes"
            :key="item.id"
            :to="item.to"
            class="px-2 sm:px-2.5 py-1 rounded text-sm hover:text-highlighted"
            :class="item.active ? 'bg-elevated text-highlighted' : 'text-muted'"
            :aria-current="item.active ? 'page' : undefined"
          >
            {{ item.label }}
          </RouterLink>
        </nav>
        <span class="flex-1" />
        <CommandPalette />
        <span class="flex-1" />
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
        <!-- The content column. It was 72rem, which is a comfortable measure
             for prose and too narrow for a table: every list on the platform
             has a commit subject, a phase, a duration and a time in it, and at
             72rem the subject is clipped and the rest is behind a horizontal
             scrollbar. The screens here are dashboards before they are
             documents, so the column is wide enough to hold a table and
             capped where a line of text would stop being readable. -->
        <div class="max-w-[110rem] mx-auto px-4 sm:px-6 py-5 sm:py-6 space-y-5">
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
          <!-- The address named a screen that is a project's, and no project.
               Rather than guessing one or refusing, the picker asks and the
               fleet dashboard renders underneath — so the address somebody
               pasted still says what they meant. -->
          <UAlert
            v-if="picker && !pickerOpen"
            color="neutral"
            variant="soft"
            icon="i-lucide-folder-search"
            title="This screen is a project's"
            :description="`${route.path} does not name one yet.`"
          >
            <template #actions>
              <UButton size="xs" color="neutral" variant="subtle" @click="pickerOpen = true">Choose a project</UButton>
            </template>
          </UAlert>
          <slot />
        </div>
      </main>
    </div>

    <UModal
      :open="Boolean(picker) && pickerOpen"
      title="Which project?"
      :description="`${route.path} is a project's screen. Choosing one finishes the address; the question it was asked with is kept.`"
      @update:open="(open: boolean) => (pickerOpen = open)"
    >
      <template #body>
        <div class="space-y-0.5 max-h-96 overflow-y-auto">
          <button
            v-for="project in projects"
            :key="project.name"
            class="w-full flex items-center gap-2.5 px-2.5 py-1.5 rounded-md text-sm text-toned hover:bg-elevated hover:text-highlighted"
            @click="chooseProject(project.name)"
          >
            <StatusDot :tone="projectTone(project.name)" />
            <span class="truncate">{{ project.name }}</span>
          </button>
          <p v-if="inventory.data.value && !projects.length" class="px-2.5 py-1.5 text-xs text-dimmed">
            No projects yet — there is nothing for this screen to be about.
          </p>
        </div>
      </template>
    </UModal>
  </div>
</template>
