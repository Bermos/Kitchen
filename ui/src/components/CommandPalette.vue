<script setup lang="ts">
import { onMounted, onUnmounted, ref, shallowRef } from "vue";
import { useRouter } from "vue-router";
import { api, type Build, type Domain, type Environment, type Project } from "../lib/api";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";

// The mockup's "Jump to project, release, domain… ⌘K": one palette over
// everything the API can list, fetched fresh when it opens.

const router = useRouter();
const open = ref(false);
const loading = ref(false);
const inventory = shallowRef<{
  projects: Project[];
  environments: Environment[];
  builds: Build[];
  domains: Domain[];
} | null>(null);

async function show() {
  open.value = true;
  loading.value = true;
  try {
    const [projects, environments, builds, domains] = await Promise.all([
      api.projects(),
      api.environments(),
      api.builds(),
      api.domains(),
    ]);
    inventory.value = { projects, environments, builds, domains };
  } finally {
    loading.value = false;
  }
}

function onKey(event: KeyboardEvent) {
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
    event.preventDefault();
    if (open.value) {
      open.value = false;
    } else {
      void show();
    }
  }
}
onMounted(() => window.addEventListener("keydown", onKey));
onUnmounted(() => window.removeEventListener("keydown", onKey));

function go(to: { name: string; params?: Record<string, string>; query?: Record<string, string> }) {
  open.value = false;
  void router.push(to);
}

const groups = () => {
  const inv = inventory.value;
  if (!inv) return [];
  return [
    {
      id: "projects",
      label: "Projects",
      items: inv.projects.map((p) => ({
        label: p.name,
        suffix: p.repo,
        icon: "i-lucide-folder-git-2",
        onSelect: () => go({ name: "project", params: { name: p.name } }),
      })),
    },
    {
      id: "environments",
      label: "Environments",
      items: inv.environments.map((e) => ({
        label: e.name,
        suffix: e.url || e.type,
        icon: "i-lucide-globe",
        onSelect: () => go({ name: "environment", params: { name: e.name } }),
      })),
    },
    {
      id: "builds",
      label: "Builds",
      // The newest fifty are the ones anyone jumps to.
      items: inv.builds.slice(0, 50).map((b) => ({
        label: b.git.message || b.name,
        suffix: b.name,
        icon: "i-lucide-hammer",
        onSelect: () => go({ name: "build", params: { name: b.name } }),
      })),
    },
    {
      id: "domains",
      label: "Domains",
      items: inv.domains.map((d) => ({
        label: d.hostname,
        suffix: d.environment,
        icon: "i-lucide-link",
        onSelect: () => go({ name: "environment", params: { name: d.environment } }),
      })),
    },
    {
      id: "pages",
      label: "Pages",
      // The pages this account has. A palette is a list of things to jump to,
      // so an entry the router would turn round at the door does not belong
      // on it — the same rule the sidebar follows, asked of the same table.
      items: [
        { label: "Overview", icon: "i-lucide-layout-dashboard", onSelect: () => go({ name: "overview" }) },
        { label: "Builds", icon: "i-lucide-hammer", onSelect: () => go({ name: "builds" }) },
        { label: "Observability", icon: "i-lucide-activity", onSelect: () => go({ name: "observability" }) },
        ...(may("GET /api/v1/connections/{name}", callerFor())
          ? [{ label: "Connections", icon: "i-lucide-plug", onSelect: () => go({ name: "connections" }) }]
          : []),
        ...(may("GET /api/v1/settings", callerFor())
          ? [{ label: "Settings", icon: "i-lucide-settings-2", onSelect: () => go({ name: "settings" }) }]
          : []),
      ],
    },
  ];
};
</script>

<template>
  <!-- Narrow enough and the trigger is the magnifier alone: the prompt and the
       shortcut are both for a screen with a keyboard in front of it. -->
  <button
    class="flex items-center gap-2 max-w-full px-2 md:px-3 py-1.5 rounded-md border border-default bg-muted text-sm text-dimmed hover:border-accented hover:text-muted md:w-80"
    aria-label="Search"
    @click="show"
  >
    <UIcon name="i-lucide-search" class="size-3.5 shrink-0" />
    <span class="hidden md:block flex-1 text-left truncate">Jump to project, environment, build…</span>
    <UKbd size="sm" class="hidden md:inline-flex">⌘K</UKbd>
  </button>

  <UModal v-model:open="open">
    <template #content>
      <UCommandPalette
        :groups="groups()"
        :loading="loading"
        placeholder="Jump to project, environment, build, domain…"
        class="h-96"
        @update:open="open = $event"
      />
    </template>
  </UModal>
</template>
