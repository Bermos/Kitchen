<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute } from "vue-router";
import type { RouteLocationRaw } from "vue-router";
import { api, type Claim, type PlatformEvent, type ServiceBinding } from "../lib/api";
import { incidentsFrom, undismissed } from "../lib/attention";
import { claimPlan, claimUsedBy, host, processRows } from "../lib/project";
import { bindingRequestSentence, claimRefusal, isBindingRequest } from "../lib/claims";
import { compactCount, timeAgo } from "../lib/format";
import { useFreshness } from "../lib/freshness";
import { buildLink, environmentLink } from "../lib/links";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { useAsync, usePoll } from "../lib/useAsync";
import AttentionBand from "../components/AttentionBand.vue";
import BindingRequestsPanel from "../components/BindingRequestsPanel.vue";
import ConditionsTable from "../components/ConditionsTable.vue";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
import SourceLink from "../components/SourceLink.vue";
import StatusDot from "../components/StatusDot.vue";

// Is this project healthy, what is it running, and what is it running on.
//
// The first of the six screens `/projects/:name` became (#470). It answers the
// question somebody opens a project with and nothing else: what is wrong, what
// this project deploys, what it depends on, and what has happened to it lately.
//
// **What it deploys and what it depends on are two tables, not one list.** A
// process has a release, replicas and something addressing it; an attached
// resource has a plan and a state. They behaved nothing alike and were rendered
// alike, which is why a project that is down because its database is down had
// no screen showing both facts side by side — even though both were already
// fetched into the one component this replaces.
//
// Everything that can be *changed* is on Settings, everything that shipped is
// on Deploys, and this screen writes nothing except the two buttons in its
// header, which are the two ways of asking for the same thing: build the
// commit again, or ask the registry what the tag names now.

const route = useRoute();
const toast = useToast();
const name = computed(() => route.params.name as string);

const { data, error, loading, refresh } = useAsync(async () => {
  const [project, environments, builds, claims] = await Promise.all([
    api.project(name.value),
    api.projectEnvironments(name.value),
    api.projectBuilds(name.value),
    api.claims({ project: name.value }),
  ]);
  return { project, environments, builds, claims };
});
watch(name, () => void refresh());

// The counters and the feed ride separately, so a project still renders on an
// installation with no telemetry store — and so that a slow feed does not hold
// up the answer to "is this healthy".
const metrics = useAsync(() => api.metricsOverview(name.value));
const activity = useAsync(() => api.events({ project: name.value, limit: 20 }));
// Who has asked to bind this project's offerings (#495). It rides separately
// like the feed, because a project that offers nothing asks for it and gets
// an empty list, and neither that nor a slow answer should hold up "is this
// healthy".
//
// It is asked for only by somebody who may read it. The queue is the
// deciders' — it carries the name of the account that asked, so the API gives
// it to this project's admins alone — and a screen that requested it for
// everybody would spend every poll being refused.
// Not `immediate`: the role it is gated on is read off the project, which is
// still loading here, so the first fetch is the one the role's own watcher
// makes below once there is an answer.
const requests = useAsync(async () => (mayDecideRequests.value ? await api.bindingRequests(name.value) : []), {
  immediate: false,
});
watch(name, () => {
  void metrics.refresh();
  void activity.refresh();
  void requests.refresh();
});

const project = computed(() => data.value?.project);
const production = computed(() =>
  data.value?.environments.find((environment) => environment.name === project.value?.productionEnvironment),
);

// What the production environment is actually running, which is the only place
// replica counts and a service's in-platform address exist. It is asked for
// only once there is a production environment to ask about.
const live = useAsync(async () => {
  const environment = production.value?.name;
  return environment ? await api.environmentProcesses(environment) : [];
});
watch(production, () => void live.refresh());

// How old this screen is, and the reader's hold on it: every fetch above
// reports into it and the header renders it.
const freshness = useFreshness();
usePoll(() => void refresh(), 15000, () => true);
usePoll(() => void live.refresh(), 15000, () => Boolean(production.value));
usePoll(() => void metrics.refresh(), 60000, () => true);
usePoll(() => void activity.refresh(), 30000, () => true);
usePoll(() => void requests.refresh(), 30000, () => mayDecideRequests.value);

const caller = computed(() => callerFor(project.value?.role, name.value));
const mayBuild = computed(() => may("POST /api/v1/projects/{name}/builds", caller.value));
// Reading the binding queue and deciding one are the same role, deliberately:
// it is the deciders' inbox and it names the person who asked. The role is
// not known until the project has loaded, so the queue is asked for again
// once it is.
const mayDecideRequests = computed(() => may("GET /api/v1/projects/{name}/requests", caller.value));
watch(mayDecideRequests, (may) => {
  if (may) void requests.refresh();
});
// Acquiring an image somebody else built is admin's where a rebuild is a
// developer's, and it is a different route, so it gets its own answer from the
// same table rather than sharing the one above.
const mayAcquire = computed(() => may("POST /api/v1/projects/{name}/acquisitions", caller.value));

const vendoredImage = computed(() => project.value?.image);
const builtHere = computed(() => Boolean(project.value?.repo));
const framework = computed(() => data.value?.builds.find((build) => build.detectedFramework)?.detectedFramework);
const previews = computed(() =>
  (data.value?.environments ?? []).filter((environment) => environment.type === "preview"),
);

// Whatever is wrong with this project, above everything else on the screen —
// the same derivation the fleet overview's band reads, narrowed to one project
// rather than re-derived. Conditions are a fact about your own project and are
// shown to every member: they were gated in four places, so a developer whose
// project was unhappy could not read the statement saying why (#469).
const incidents = computed(() => {
  const loaded = data.value;
  if (!loaded) return [];
  return incidentsFrom([loaded.project], loaded.environments, loaded.builds);
});
const attention = computed(() => undismissed(incidents.value));

/**
 * This project's own counters — `/metrics/overview?project=` is the same
 * answer as the fleet's, narrowed, so these are this project's numbers rather
 * than the platform's with a filter drawn over them.
 *
 * A traffic number reads `—` where nothing was measured rather than `0`: an
 * environment the platform's edge does not reach and one nobody visited are
 * different answers, and only one of them is a zero.
 */
const counters = computed(() => {
  const m = metrics.data.value;
  const measured = Boolean(m && (m.requests24h > 0 || m.requestsPerHour.some((value) => value > 0)));
  return [
    { label: "Release", value: production.value?.release || "—", mono: true },
    { label: "Previews", value: String(previews.value.length), mono: false },
    { label: "Requests · 24 h", value: measured ? compactCount(m!.requests24h) : "—", mono: false },
    { label: "p95", value: measured && m!.p95Ms24h > 0 ? `${Math.round(m!.p95Ms24h)} ms` : "—", mono: false },
  ];
});

const processes = computed(() =>
  project.value ? processRows(project.value, production.value, live.data.value ?? []) : [],
);
const claims = computed(() => data.value?.claims ?? []);
// A binding the providing project refused is `Failed` too, and its story is
// the request row's below — with the provider's own words and the one act
// this side has — so it is left to that one rather than said twice (#495).
const refusedClaims = computed(() =>
  claims.value.filter((claim) => claim.phase === "Failed" && !isBindingRequest(claim)),
);

// A binding to another project's offering does not reach one address: it
// reaches whichever environment of the provider admits the class of
// environment asking, which its owners decide. So a preview of this project
// and its production can reach two different environments of the provider —
// or a preview can reach nothing, which is the answer this row exists to make
// visible rather than leaving somebody to find out from a missing variable.
const boundServiceClaims = computed(() => claims.value.filter((claim) => claim.service?.bindings?.length));

/** Which classes of this project's environments reach nothing, with why. */
function unreached(claim: Claim): ServiceBinding[] {
  return (claim.service?.bindings ?? []).filter((binding) => !binding.environment);
}

// The other side of a binding request (#495), read here rather than on the
// provider's screen: what this project asked another project for, and what
// came back. A refused binding carries that project's own words, which is the
// only place this reader can see them — they hold no role over there.
const askedFor = computed(() => claims.value.filter(isBindingRequest));

// Asking again after a refusal, on the same claim: the record of who asked
// and who refused stays, which deleting the claim and writing it afresh would
// throw away.
const mayAskAgain = computed(() => may("POST /api/v1/claims/{name}/request", caller.value));
const asking = ref("");
async function askAgain(claim: Claim) {
  if (asking.value) return;
  asking.value = claim.name;
  try {
    await api.requestBinding(claim.name);
    toast.add({
      title: `${claim.service?.project} has been asked again`,
      description: "Nothing binds until they answer it.",
      color: "success",
      icon: "i-lucide-check",
    });
    await refresh();
  } catch (err) {
    toast.add({
      title: err instanceof Error ? err.message : String(err),
      color: "warning",
      icon: "i-lucide-info",
    });
  } finally {
    asking.value = "";
  }
}

// What a feed entry links to: the most specific object it names.
function eventTarget(event: PlatformEvent): RouteLocationRaw | null {
  if (event.build) return buildLink(event.build, event.project);
  if (event.environment) return environmentLink(event.environment, event.project);
  return null;
}

function eventTone(event: PlatformEvent): string {
  if (event.type === "build.failed" || event.type === "claim.failed" || event.type === "run.failed") {
    return "text-error";
  }
  if (event.type === "release.rolledBack") return "text-warning";
  return "text-muted";
}

// Rebuild: POST /projects/{name}/builds with an empty body repeats the last
// commit — a rerun after a flaky build or a changed secret.
const redeploying = ref(false);
async function redeploy() {
  redeploying.value = true;
  try {
    const build = await api.rebuild(name.value);
    toast.add({ title: `Build ${build.name} queued`, color: "success", icon: "i-lucide-hammer" });
    await refresh();
  } catch (err) {
    toast.add({
      title: "Rebuild failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    redeploying.value = false;
  }
}

// Acquire: the vendored equivalent of a redeploy. A project with no repository
// has no commit to rebuild, and what moves it is a new digest under the tag it
// follows — which the platform asks about on an interval and this asks about
// now, for somebody who has just published upstream and would rather not wait.
const acquiring = ref(false);
async function acquire() {
  acquiring.value = true;
  try {
    const build = await api.acquire(name.value);
    toast.add({
      title: `Acquisition ${build.name} queued`,
      description: "The registry is being asked what this tag names now.",
      color: "success",
      icon: "i-lucide-package-search",
    });
    await refresh();
  } catch (err) {
    toast.add({
      title: "Acquisition failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    acquiring.value = false;
  }
}
</script>

<template>
  <div class="space-y-6">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <template v-else-if="project">
      <PageHeader
        :freshness="freshness"
        :title="project.name"
        :breadcrumb="[{ label: 'Projects', to: '/projects' }, { label: project.name, mono: true }, { label: 'Overview' }]"
      >
        <template #meta>
          <SourceLink v-if="project.repo" :href="project.repositoryUrl">{{ project.repo }}</SourceLink>
          <span v-else-if="vendoredImage" class="font-mono">{{ vendoredImage.reference }}</span>
          <a
            v-if="production?.url"
            :href="production.url"
            target="_blank"
            rel="noopener"
            class="font-mono text-primary hover:underline"
            >{{ host(production.url) }}</a
          >
          <!-- No address, and not because something failed to publish one:
               this project asked for none. Said here, where the address would
               have been, rather than left as a gap. -->
          <span
            v-else-if="project.exposure === 'internal'"
            class="inline-flex items-center gap-1"
            title="This project is internal: no environment of it has a hostname or a certificate. They are reachable from the other applications on this platform."
          >
            <UIcon name="i-lucide-shield" class="size-3" />internal
          </span>
          <span v-if="framework && builtHere" class="inline-flex items-center gap-1">
            <UIcon name="i-lucide-sparkles" class="size-3" />{{ framework }}, detected
          </span>
          <span v-if="builtHere" class="font-mono">{{ project.productionBranch }}</span>
        </template>
        <template #actions>
          <UButton
            v-if="builtHere && mayBuild"
            color="neutral"
            variant="subtle"
            size="sm"
            icon="i-lucide-rotate-cw"
            :loading="redeploying"
            @click="redeploy"
          >
            Redeploy
          </UButton>
          <!-- A project with no repository has no commit to rebuild, so the
               button that moves it is the other one: ask the registry what the
               tag it follows names now, and take it. -->
          <UButton
            v-if="vendoredImage && mayAcquire"
            color="neutral"
            variant="subtle"
            size="sm"
            icon="i-lucide-package-search"
            :loading="acquiring"
            @click="acquire"
          >
            Check for a new digest
          </UButton>
          <UButton
            v-if="production?.url"
            :href="production.url"
            target="_blank"
            size="sm"
            icon="i-lucide-arrow-up-right"
            trailing
          >
            Visit site
          </UButton>
        </template>
      </PageHeader>

      <!-- Whatever is broken, above everything else: the error at its full
           length, what it costs, and the way out. -->
      <AttentionBand :incidents="attention" @acted="refresh" />

      <div class="grid grid-cols-2 lg:grid-cols-4 gap-3">
        <div v-for="counter in counters" :key="counter.label" class="rounded-md border border-default px-3 py-2">
          <p class="text-[11px] text-muted truncate">{{ counter.label }}</p>
          <p
            class="text-sm font-semibold text-highlighted truncate"
            :class="counter.mono ? 'font-mono' : 'tabular-nums'"
            :title="counter.value"
          >
            {{ counter.value }}
          </p>
        </div>
      </div>

      <!-- What this project deploys. The word is processes: `service` is one of
           the four types and means something narrower than the table, so a
           table headed Services would contradict three of its own rows. -->
      <PageSection
        title="Processes"
        description="What this project runs, and who can reach each of it. The unit deploys as one, so every row carries the same release."
      >
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[42rem] text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default bg-muted">
                <th class="px-3 py-2 font-medium">Process</th>
                <th class="px-3 py-2 font-medium">Type</th>
                <th class="px-3 py-2 font-medium">Release</th>
                <th class="px-3 py-2 font-medium">Replicas</th>
                <th class="px-3 py-2 font-medium">Addressed from</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="row in processes" :key="row.name" class="border-b border-muted last:border-0">
                <td class="px-3 py-2 font-mono text-highlighted">{{ row.name }}</td>
                <td class="px-3 py-2">
                  <UBadge color="neutral" variant="subtle" size="sm" class="font-mono">{{ row.type }}</UBadge>
                </td>
                <td class="px-3 py-2 font-mono text-xs text-toned">{{ row.release || "—" }}</td>
                <td class="px-3 py-2 font-mono text-xs text-toned">{{ row.replicas || "—" }}</td>
                <td class="px-3 py-2 text-xs">
                  <a
                    v-if="row.url"
                    :href="row.url"
                    target="_blank"
                    rel="noopener"
                    class="font-mono text-primary hover:underline"
                    >{{ host(row.url) }}</a
                  >
                  <span v-else class="text-muted">{{ row.addressedFrom }}</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </PageSection>

      <!-- And what it depends on. Not deployables, and deliberately not in the
           table above — but a project that is down because its database is down
           needs both facts on one screen. -->
      <PageSection
        title="Attached resources"
        description="What this project claimed. Asking for one, and giving one up, is on Settings."
      >
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[42rem] text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default bg-muted">
                <th class="px-3 py-2 font-medium">Resource</th>
                <th class="px-3 py-2 font-medium">Type</th>
                <th class="px-3 py-2 font-medium">Plan</th>
                <th class="px-3 py-2 font-medium">State</th>
                <th class="px-3 py-2 font-medium">Used by</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!claims.length">
                <td colspan="5" class="px-3 py-8 text-center text-muted">
                  Nothing attached — a claim asks for something this project needs (a database or a bucket from a
                  connection, single sign-on from the platform's own identity provider, durable background work, or
                  storage for one process) and binds it in.
                </td>
              </tr>
              <tr v-for="claim in claims" :key="claim.name" class="border-b border-muted last:border-0">
                <td class="px-3 py-2 text-highlighted font-medium">{{ claim.name }}</td>
                <td class="px-3 py-2">
                  <UBadge color="neutral" variant="subtle" size="sm" class="font-mono">{{ claim.type }}</UBadge>
                </td>
                <td class="px-3 py-2 font-mono text-xs text-toned">{{ claimPlan(claim) }}</td>
                <td class="px-3 py-2"><PhaseBadge :phase="claim.phase" /></td>
                <td class="px-3 py-2 text-xs text-muted">{{ claimUsedBy(claim) }}</td>
              </tr>
              <!-- Why a claim was refused, in the provider's own words. A claim
                   that cannot be satisfied fails here rather than in the
                   application three minutes later, and that is only worth
                   anything if the reason is where the claim is. -->
              <tr v-for="claim in refusedClaims" :key="`${claim.name}-why`" class="border-b border-muted last:border-0">
                <td colspan="5" class="px-3 py-2 text-xs text-error">
                  <span class="font-mono">{{ claim.name }}</span> — {{ claimRefusal(claim) }}
                </td>
              </tr>
              <!-- A binding this project asked another project for and has
                   not been given (#495). It is not a failure of this project's
                   and there is nothing here to fix: the grant belongs to the
                   other project's admins, so the row says what is true and
                   offers the one act this side has — asking again. -->
              <tr v-for="claim in askedFor" :key="`${claim.name}-asked`" class="border-b border-muted last:border-0">
                <td colspan="5" class="px-3 py-2 text-xs">
                  <div class="flex items-start justify-between gap-4">
                    <p class="text-muted">
                      <span class="font-mono">{{ claim.name }}</span> — {{ bindingRequestSentence(claim) }}
                    </p>
                    <UButton
                      v-if="mayAskAgain && claim.service?.grant?.state === 'denied'"
                      color="neutral"
                      variant="link"
                      size="xs"
                      class="px-0 shrink-0"
                      :loading="asking === claim.name"
                      @click="askAgain(claim)"
                    >
                      Ask again
                    </UButton>
                  </div>
                </td>
              </tr>
              <!-- What a binding to another project's offering reaches, per
                   class of this project's own environments. The providing
                   project's environment owners decide who may bind to each of
                   theirs, so this is where a preview reaching nothing — or
                   reaching a staging environment rather than production — is
                   said out loud. -->
              <tr
                v-for="claim in boundServiceClaims"
                :key="`${claim.name}-reaches`"
                class="border-b border-muted last:border-0"
              >
                <td colspan="5" class="px-3 py-2 text-xs">
                  <p class="text-muted">
                    <span class="font-mono">{{ claim.name }}</span>
                    <template v-for="(binding, i) in claim.service!.bindings" :key="binding.consumer">
                      {{ i ? "·" : "—" }} {{ binding.consumer }} reaches
                      <span v-if="binding.environment" class="font-mono text-toned">{{ binding.environment }}</span>
                      <span v-else class="text-dimmed">nothing</span>
                    </template>
                  </p>
                  <p v-for="binding in unreached(claim)" :key="`${binding.consumer}-why`" class="text-dimmed mt-1">
                    {{ binding.consumer }}: {{ binding.reason }}
                  </p>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </PageSection>

      <!-- Who has asked to bind what this project offers (#495). It is here
           rather than on Settings because it is a queue somebody answers
           rather than a setting somebody changes, and because this is the
           screen its admins already open. -->
      <BindingRequestsPanel
        v-if="requests.data.value?.length"
        :project="name"
        :role="project.role"
        :requests="requests.data.value ?? []"
        @decided="requests.refresh()"
      />

      <!-- Everybody's since #469: conditions are a fact about this project. -->
      <ConditionsTable :conditions="project.conditions" />

      <!-- This project only, so the feed is short enough to actually read. -->
      <PageSection title="Recent activity" description="Deploys, promotions, claims and member changes on this project.">
        <div class="rounded-md border border-default">
          <p v-if="!activity.data.value?.length" class="px-4 py-8 text-center text-sm text-muted">
            {{ activity.loading.value ? "Loading…" : "Nothing has happened here yet." }}
          </p>
          <ul v-else class="divide-y divide-muted">
            <li v-for="(event, i) in activity.data.value" :key="i" class="px-4 py-2 flex items-center gap-3 text-sm">
              <StatusDot :tone="eventTone(event) === 'text-error' ? 'error' : 'neutral'" />
              <component
                :is="eventTarget(event) ? 'RouterLink' : 'span'"
                v-bind="eventTarget(event) ? { to: eventTarget(event) } : {}"
                class="flex-1 min-w-0 truncate"
                :class="[eventTone(event), eventTarget(event) ? 'hover:text-highlighted hover:underline' : '']"
              >
                {{ event.message }}
              </component>
              <span v-if="event.actor && event.actor !== 'operator'" class="text-xs text-dimmed shrink-0">
                {{ event.actor }}
              </span>
              <span class="text-xs text-muted shrink-0 tabular-nums">{{ timeAgo(event.timestamp) }}</span>
            </li>
          </ul>
        </div>
      </PageSection>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>
  </div>
</template>
