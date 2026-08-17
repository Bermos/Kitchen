<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { api, type Certificate } from "../lib/api";
import { compactCount, timeAgo } from "../lib/format";
import { CERT_EXPIRY_DAYS } from "../lib/platform";
import { formatLatency, formatPercent, formatRate, type SignalTile } from "../lib/requests";
import { useAsync, usePoll } from "../lib/useAsync";
import EdgeRanking from "../components/EdgeRanking.vue";
import GoldenSignals from "../components/GoldenSignals.vue";
import StatusDot from "../components/StatusDot.vue";

// The front door: what it served across every project, and whether the door
// itself is in one piece.
//
// The two halves fail independently and are drawn that way. The traffic tables
// need the telemetry store; the Gateway, the tunnel and the certificates are
// objects the API server holds, and an installation without telemetry still has
// a Gateway worth looking at — so the store's absence is a sentence over the
// tables rather than an error page over the screen.
//
// The certificate table is the other half of this screen, and `message` is the
// most useful string on it: for a stuck ACME order it is the error the CA
// returned, verbatim, which is the one thing that says what to fix. It is never
// paraphrased here.

const route = useRoute();
/** Where `edge.unrouted-hosts` and `cert.expiring` evidence lands. */
const host = computed(() => (route.query.host as string) || "");

const ranges = [
  { label: "Last 15 minutes", value: 15 },
  { label: "Last hour", value: 60 },
  { label: "Last 6 hours", value: 360 },
  { label: "Last 24 hours", value: 1440 },
  { label: "Last 7 days", value: 10080 },
];
const rangeMinutes = ref(60);
const windowStart = ref(new Date(Date.now() - rangeMinutes.value * 60_000).toISOString());

const { data, error, loading, refresh } = useAsync(() => api.platformEdge({ since: windowStart.value, limit: 10 }));

function reload() {
  windowStart.value = new Date(Date.now() - rangeMinutes.value * 60_000).toISOString();
  void refresh();
}
watch(rangeMinutes, reload);
usePoll(reload, 60_000, () => true);

const traffic = computed(() => data.value?.requests);
const trafficMessage = computed(() => data.value?.trafficMessage ?? "");

const tiles = computed<SignalTile[]>(() => {
  const answer = traffic.value;
  return [
    {
      label: "Requests",
      value: answer ? formatRate(answer.requestsPerSecond) : "—",
      detail: answer ? `${compactCount(answer.requests)} entered the platform in this window` : trafficMessage.value,
    },
    {
      label: "Error rate",
      value: answer ? formatPercent(answer.errorRate) : "—",
      detail: answer ? `${compactCount(answer.errors)} answered 500 or above` : "",
      tone: "text-error",
    },
    {
      label: "p95 latency",
      value: formatLatency(answer?.p95Ms),
      detail: answer ? `p50 ${formatLatency(answer.p50Ms)} · p99 ${formatLatency(answer.p99Ms)}` : "",
    },
    {
      label: "Unrouted",
      value: answer ? compactCount(answer.unrouted) : "—",
      detail: "asked for a hostname this platform never published",
      tone: "text-warning",
    },
  ];
});

const certificates = computed(() => data.value?.certificates?.items ?? []);

function certificateTone(certificate: Certificate) {
  if (!certificate.ready) return "error" as const;
  if ((certificate.daysToExpiry ?? Infinity) <= CERT_EXPIRY_DAYS) return "warning" as const;
  if (certificate.issuing) return "warning" as const;
  return "success" as const;
}

function expiry(certificate: Certificate): string {
  if (certificate.daysToExpiry === undefined) return "—";
  const days = certificate.daysToExpiry;
  if (days < 1) return `${Math.max(Math.round(days * 24), 0)} h`;
  return `${Math.round(days)} d`;
}
</script>

<template>
  <div class="space-y-5">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <div class="flex items-center gap-2 text-xs text-muted mb-1">
          <RouterLink to="/platform" class="hover:text-highlighted">Platform</RouterLink>
          <span>/</span>
          <span class="text-toned">Edge</span>
        </div>
        <h1 class="text-xl font-semibold text-highlighted">Edge</h1>
        <p class="text-xs text-muted mt-1">
          Everything that entered the platform, across every project — and the Gateway, the tunnel and the certificates
          it entered through.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <USelect v-model="rangeMinutes" :items="ranges" size="xs" class="w-40" />
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="loading"
          aria-label="Refresh"
          @click="reload"
        />
      </div>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <template v-else>
      <UAlert
        v-if="trafficMessage"
        color="neutral"
        variant="soft"
        icon="i-lucide-database-zap"
        title="No traffic numbers on this installation"
        :description="`${trafficMessage} The Gateway, the tunnel and the certificates below are read from the API server and are unaffected.`"
      />

      <GoldenSignals :tiles="tiles" />
      <p v-if="traffic" class="text-[11px] text-dimmed">
        Answered over {{ timeAgo(traffic.since) }} to now off the {{ traffic.rollup }} rollup. A platform-wide p95 is a
        merge over every project's states, not the largest of theirs — percentiles do not add up.
      </p>

      <div v-if="!trafficMessage" class="grid gap-4 lg:grid-cols-2">
        <EdgeRanking
          title="Busiest routes"
          hint="by requests"
          :entries="data?.topRoutes"
          by="requests"
          :loading="loading"
          empty="Nothing was served in this window."
        />
        <EdgeRanking
          title="Worst routes"
          hint="by error rate, with too-quiet rows dropped"
          :entries="data?.worstRoutes"
          by="errorRate"
          :loading="loading"
          empty="No route had enough traffic to be ranked by failure."
        />
        <EdgeRanking
          title="Busiest hosts"
          hint="by requests"
          :entries="data?.topHosts"
          by="requests"
          :loading="loading"
          empty="No host was asked for in this window."
        />
        <EdgeRanking
          title="Worst hosts"
          hint="by error rate"
          :entries="data?.worstHosts"
          by="errorRate"
          :loading="loading"
          empty="No host had enough traffic to be ranked by failure."
        />
        <EdgeRanking
          title="Latency leaders"
          hint="by p95"
          :entries="data?.latencyLeaders"
          by="p95"
          :loading="loading"
          empty="Nothing was slow enough to rank."
          class="lg:col-span-2"
        />
      </div>

      <div v-if="!trafficMessage">
        <h2 class="text-sm font-medium text-highlighted mb-2">
          Unrouted hosts
          <span class="text-dimmed font-normal text-xs"
            >— asked for at this edge and never published by this platform</span
          >
        </h2>
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default bg-muted">
                <th class="px-4 py-2 font-medium">Host</th>
                <th class="px-4 py-2 font-medium text-right">Requests</th>
                <th class="px-4 py-2 font-medium">First seen</th>
                <th class="px-4 py-2 font-medium">Last seen</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!data?.unrouted?.length">
                <td colspan="4" class="px-4 py-4 text-center text-xs text-muted">
                  Every request in the window asked for a hostname this platform publishes.
                </td>
              </tr>
              <tr
                v-for="entry in data?.unrouted ?? []"
                :key="entry.host"
                class="border-b border-muted last:border-0"
                :class="host && entry.host === host ? 'bg-warning/10' : ''"
              >
                <td class="px-4 py-2 font-mono text-xs text-highlighted break-all">{{ entry.host || "(none)" }}</td>
                <td class="px-4 py-2 text-right font-mono text-xs tabular-nums text-toned">
                  {{ compactCount(entry.requests) }} <span class="text-dimmed">· {{ formatRate(entry.requestsPerSecond) }}</span>
                </td>
                <td class="px-4 py-2 text-xs text-dimmed whitespace-nowrap">{{ timeAgo(entry.firstSeen) }}</td>
                <td class="px-4 py-2 text-xs text-dimmed whitespace-nowrap">{{ timeAgo(entry.lastSeen) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <p class="text-[11px] text-dimmed mt-1.5">
          A host asked for once an hour ago is a scanner; one asked for continuously since a deploy is a route that
          stopped being published. This table is read over its own window rather than the one above, because “still
          asking” is a question about a stretch of time.
        </p>
      </div>

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">Gateways</h2>
        <div class="rounded-md border border-default divide-y divide-default overflow-hidden">
          <p v-if="!data?.gateways?.length" class="px-4 py-3 text-xs text-muted">
            No Gateway was found — nothing on this platform is published.
          </p>
          <div v-for="gateway in data?.gateways ?? []" :key="`${gateway.namespace}/${gateway.name}`" class="px-4 py-3">
            <div class="flex items-center gap-2 flex-wrap">
              <StatusDot :tone="gateway.programmed && gateway.accepted ? 'success' : 'error'" />
              <span class="font-mono text-sm text-highlighted">{{ gateway.name }}</span>
              <span class="text-xs text-dimmed font-mono">{{ gateway.namespace }}</span>
              <UBadge v-if="gateway.class" color="neutral" variant="subtle" size="sm">{{ gateway.class }}</UBadge>
              <span class="flex-1" />
              <span class="font-mono text-xs text-toned">{{ (gateway.addresses ?? []).join(", ") || "no address" }}</span>
            </div>
            <p v-if="gateway.message" class="text-xs mt-1" :class="gateway.programmed ? 'text-muted' : 'text-error'">
              {{ gateway.message }}
            </p>
            <table v-if="gateway.listeners?.length" class="w-full text-xs mt-2">
              <tbody>
                <tr v-for="listener in gateway.listeners" :key="listener.name" class="align-top">
                  <td class="py-1 pr-3 font-mono text-toned whitespace-nowrap">
                    <span class="inline-flex items-center gap-1.5">
                      <StatusDot :tone="listener.programmed ? 'success' : 'error'" />
                      {{ listener.name }}
                    </span>
                  </td>
                  <td class="py-1 pr-3 font-mono text-dimmed whitespace-nowrap">{{ listener.protocol }}:{{ listener.port }}</td>
                  <td class="py-1 pr-3 font-mono text-dimmed whitespace-nowrap">{{ listener.attachedRoutes }} routes</td>
                  <td class="py-1 text-toned w-full break-words">{{ listener.message || "" }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
        <p class="text-[11px] text-dimmed mt-1.5">
          A Gateway with no address is <span class="font-mono">Programmed=False</span> — cloudflared removes the need
          for that address to be <em>routable</em>, never the need for it to exist.
        </p>
      </div>

      <div v-if="data?.tunnel">
        <h2 class="text-sm font-medium text-highlighted mb-2">Tunnel</h2>
        <div
          class="rounded-md border px-4 py-3 flex items-center gap-3 flex-wrap"
          :class="data.tunnel.healthy ? 'border-default' : 'border-error/40 bg-error/5'"
        >
          <StatusDot :tone="data.tunnel.healthy ? 'success' : 'error'" />
          <span class="font-mono text-sm text-highlighted">{{ data.tunnel.name }}</span>
          <span class="font-mono text-xs text-toned">
            {{ data.tunnel.available }} of {{ data.tunnel.desired }} available
          </span>
          <span class="font-mono text-xs" :class="data.tunnel.restarts ? 'text-warning' : 'text-dimmed'">
            {{ data.tunnel.restarts }} restart{{ data.tunnel.restarts === 1 ? "" : "s" }}
          </span>
          <span class="flex-1" />
          <span class="text-xs" :class="data.tunnel.healthy ? 'text-muted' : 'text-error'">{{ data.tunnel.message }}</span>
        </div>
      </div>

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">Certificates</h2>
        <UAlert
          v-if="data?.certificates?.message && !certificates.length"
          color="neutral"
          variant="soft"
          icon="i-lucide-shield-off"
          title="No certificates are managed here"
          :description="data.certificates.message"
        />
        <div v-else class="rounded-md border border-default overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default bg-muted">
                <th class="px-4 py-2 font-medium">Certificate</th>
                <th class="px-4 py-2 font-medium">Names</th>
                <th class="px-4 py-2 font-medium text-right">Expires in</th>
                <th class="px-4 py-2 font-medium">Renews</th>
                <th class="px-4 py-2 font-medium">State</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!certificates.length">
                <td colspan="5" class="px-4 py-4 text-center text-xs text-muted">
                  {{ loading ? "Loading…" : "No certificates." }}
                </td>
              </tr>
              <tr
                v-for="certificate in certificates"
                :key="`${certificate.namespace}/${certificate.name}`"
                class="border-b border-muted last:border-0 align-top"
                :class="[
                  certificateTone(certificate) === 'error' ? 'bg-error/5' : '',
                  host && (certificate.dnsNames ?? []).some((name) => name.includes(host)) ? 'bg-warning/10' : '',
                ]"
              >
                <td class="px-4 py-2.5">
                  <span class="inline-flex items-center gap-2">
                    <StatusDot :tone="certificateTone(certificate)" />
                    <span class="font-mono text-xs text-highlighted">{{ certificate.name }}</span>
                  </span>
                  <p class="text-[11px] text-dimmed pl-3.5">{{ certificate.namespace }}</p>
                </td>
                <td class="px-4 py-2.5 font-mono text-xs text-toned break-all">
                  {{ (certificate.dnsNames ?? []).join(", ") || "—" }}
                </td>
                <td
                  class="px-4 py-2.5 text-right font-mono text-xs tabular-nums"
                  :class="(certificate.daysToExpiry ?? Infinity) <= CERT_EXPIRY_DAYS ? 'text-warning' : 'text-toned'"
                >
                  {{ expiry(certificate) }}
                </td>
                <td class="px-4 py-2.5 text-xs text-dimmed whitespace-nowrap">
                  {{ certificate.renewalTime ? timeAgo(certificate.renewalTime) : "—" }}
                </td>
                <td class="px-4 py-2.5 text-xs max-w-md">
                  <span :class="certificate.ready ? 'text-success' : 'text-error'">
                    {{ certificate.ready ? "Ready" : "Not ready" }}
                  </span>
                  <!-- Where a renewal that keeps failing reports itself: the
                       Ready condition stays true on the still-valid old
                       certificate, so this is the only place it says so. -->
                  <p v-if="certificate.issuing" class="text-warning mt-0.5">Renewing: {{ certificate.issuing }}</p>
                  <p v-if="certificate.message" class="text-toned font-mono text-[11px] mt-0.5 break-words">
                    {{ certificate.message }}
                  </p>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <p class="text-[11px] text-dimmed mt-1.5">
          A healthy certificate carries no message — “up to date and has not expired” is what Ready already said. The
          message on a stuck one is the CA's own words, and is deliberately not paraphrased. Wildcards are DNS-01 only,
          so an ACME error here is usually a DNS one.
        </p>
      </div>
    </template>
  </div>
</template>
