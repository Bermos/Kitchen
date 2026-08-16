<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type Domain } from "../lib/api";
import { timeAgo } from "../lib/format";
import { useAsync, usePoll } from "../lib/useAsync";
import ConditionsTable from "./ConditionsTable.vue";
import StatusDot from "./StatusDot.vue";

// Custom domains for one environment: attach a hostname, get the DNS record
// to create, watch verification and the certificate happen, detach with a
// confirmation. Creating a domain changes nothing by itself — DNS is the
// user's move — so the panel's job is showing exactly what that move is.

const props = defineProps<{ environment: string }>();

const toast = useToast();

const { data, error, refresh } = useAsync(() => api.domains({ environment: props.environment }));
watch(
  () => props.environment,
  () => void refresh(),
);

const domains = computed(() => data.value ?? []);
// DNS propagation, issuance and route acceptance all finish on their own
// time; poll while anything is still getting there.
const settling = computed(() =>
  domains.value.some((d) => !d.verified || d.conditions?.some((c) => c.status !== "True")),
);
usePoll(() => void refresh(), 5000, () => settling.value);

const expanded = ref<string | null>(null);
function toggle(name: string) {
  expanded.value = expanded.value === name ? null : name;
}

function domainTone(domain: Domain) {
  if (!domain.verified) return "warning" as const;
  return domain.conditions?.some((c) => c.status === "False") ? "warning" : ("success" as const);
}

function domainState(domain: Domain): string {
  if (!domain.verified) return "awaiting DNS verification";
  const pending = domain.conditions?.find((c) => c.status !== "True");
  if (!pending) return "serving";
  if (pending.type === "CertificateReady") return "issuing certificate";
  if (pending.type === "RouteProgrammed") return "programming route";
  return `waiting on ${pending.type}`;
}

function tlsLabel(domain: Domain): string {
  const mode = domain.effectiveTLS || domain.tls;
  if (!mode) return "inherits platform";
  return domain.tls ? mode : `${mode} (inherited)`;
}

async function copy(label: string, value: string) {
  try {
    await navigator.clipboard.writeText(value);
    toast.add({ title: `${label} copied`, color: "success", icon: "i-lucide-clipboard-check" });
  } catch (err) {
    toast.add({ title: "Copy failed", description: err instanceof Error ? err.message : String(err), color: "error" });
  }
}

// The attach flow: a small form, and — once created — the DNS instructions
// with live verification state, because the record is the actual work.
const adding = ref(false);
const hostname = ref("");
const tls = ref("");
const tlsOptions = [
  { label: "Inherit the platform's mode", value: "" },
  { label: "acme — certificate from the platform's CA", value: "acme" },
  { label: "cloudflared — TLS at the Cloudflare edge", value: "cloudflared" },
  { label: "none — plain HTTP", value: "none" },
];
const created = ref<Domain | null>(null);
const saving = ref(false);

watch(adding, (open) => {
  if (!open) return;
  hostname.value = "";
  tls.value = "";
  created.value = null;
});

// While the instructions are on screen, keep asking whether the record has
// landed — the moment it verifies, the modal says so.
usePoll(
  () => {
    const domain = created.value;
    if (!domain) return;
    void api.domain(domain.name).then((fresh) => {
      created.value = fresh;
      void refresh();
    });
  },
  5000,
  () => adding.value && created.value !== null && !created.value.verified,
);

async function attach() {
  if (!hostname.value || saving.value) return;
  saving.value = true;
  try {
    created.value = await api.createDomain({
      hostname: hostname.value.trim(),
      environment: props.environment,
      tls: tls.value || undefined,
    });
    toast.add({ title: `${created.value.hostname} attached`, color: "success", icon: "i-lucide-globe" });
    await refresh();
  } catch (err) {
    toast.add({
      title: "Attaching the domain failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    saving.value = false;
  }
}

const removing = ref<Domain | null>(null);
const deleting = ref(false);
async function detach() {
  const domain = removing.value;
  if (!domain) return;
  deleting.value = true;
  try {
    await api.deleteDomain(domain.name);
    toast.add({
      title: `${domain.hostname} is being detached`,
      description: "The operator removes its certificate and takes it off the gateway.",
      color: "success",
      icon: "i-lucide-trash-2",
    });
    removing.value = null;
    await refresh();
  } catch (err) {
    toast.add({
      title: "Detaching the domain failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    deleting.value = false;
  }
}
</script>

<template>
  <div>
    <div class="flex items-center justify-between mb-2">
      <h2 class="text-sm font-medium text-highlighted">Custom domains</h2>
      <UButton color="neutral" variant="subtle" size="xs" icon="i-lucide-plus" @click="adding = true">
        Add domain
      </UButton>
    </div>
    <p class="text-xs text-muted mb-3">
      Point a domain you own at this environment. Ownership is proven with a DNS record; verified domains join the
      route and, in acme mode, get their own certificate.
    </p>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <div v-else class="rounded-md border border-default divide-y divide-default overflow-hidden">
      <p v-if="!domains.length" class="px-4 py-3 text-sm text-muted">
        No custom domains — this environment is served on its generated URL alone.
      </p>
      <div v-for="domain in domains" :key="domain.name">
        <div
          class="flex items-center gap-3 px-4 py-2.5 text-sm cursor-pointer hover:bg-elevated"
          @click="toggle(domain.name)"
        >
          <StatusDot :tone="domainTone(domain)" :pulse="!domain.verified" />
          <span class="font-mono text-highlighted">{{ domain.hostname }}</span>
          <UBadge color="neutral" variant="subtle" size="sm">{{ tlsLabel(domain) }}</UBadge>
          <span class="text-xs text-muted">{{ domainState(domain) }}</span>
          <span class="ml-auto text-xs text-dimmed whitespace-nowrap">{{ timeAgo(domain.createdAt) }}</span>
          <UButton
            color="neutral"
            variant="ghost"
            size="xs"
            icon="i-lucide-trash-2"
            aria-label="Detach domain"
            @click.stop="removing = domain"
          />
        </div>
        <div v-if="expanded === domain.name" class="px-4 py-3 bg-muted space-y-3 border-t border-muted">
          <div v-if="!domain.verified && domain.verification" class="space-y-2">
            <p class="text-xs text-muted">
              Create <span class="font-medium text-toned">either</span> record in the domain's DNS zone:
            </p>
            <div class="flex items-center gap-2 text-xs font-mono">
              <span class="text-dimmed w-14 shrink-0">TXT</span>
              <span class="text-highlighted break-all">{{ domain.verification.txtRecord }}</span>
              <span class="text-dimmed">=</span>
              <span class="text-toned break-all">{{ domain.verification.txtValue }}</span>
              <UButton
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-copy"
                aria-label="Copy TXT value"
                @click="copy('TXT value', domain.verification!.txtValue)"
              />
            </div>
            <div v-if="domain.verification.cnameTarget" class="flex items-center gap-2 text-xs font-mono">
              <span class="text-dimmed w-14 shrink-0">CNAME</span>
              <span class="text-highlighted break-all">{{ domain.hostname }}</span>
              <span class="text-dimmed">→</span>
              <span class="text-toned break-all">{{ domain.verification.cnameTarget }}</span>
              <UButton
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-copy"
                aria-label="Copy CNAME target"
                @click="copy('CNAME target', domain.verification!.cnameTarget!)"
              />
            </div>
            <p class="text-xs text-dimmed">
              The CNAME also routes traffic here, which the domain needs anyway.
            </p>
          </div>
          <ConditionsTable :conditions="domain.conditions" />
        </div>
      </div>
    </div>

    <!-- Attach flow: form first, then the DNS record to create. -->
    <UModal
      :open="adding"
      :title="created ? `Verify ${created.hostname}` : 'Add a custom domain'"
      :description="
        created
          ? 'Create one of these records in the domain\'s DNS zone. This page updates itself as soon as the platform sees it.'
          : 'The domain must live in a zone you control — names under the platform\'s base domain are generated and routed already.'
      "
      @update:open="(open: boolean) => { adding = open; }"
    >
      <template #body>
        <form v-if="!created" class="space-y-4" @submit.prevent="attach">
          <UFormField label="Hostname" help="Fully qualified, e.g. shop.example.com." required>
            <UInput v-model="hostname" placeholder="shop.example.com" class="w-full font-mono" autofocus />
          </UFormField>
          <UFormField label="TLS" help="How this hostname is served. Inheriting follows the platform.">
            <USelect v-model="tls" :items="tlsOptions" class="w-full" />
          </UFormField>
        </form>
        <div v-else class="space-y-3">
          <div class="rounded-md border border-default bg-muted px-4 py-3 space-y-2 text-xs font-mono">
            <div class="flex items-center gap-2">
              <span class="text-dimmed w-14 shrink-0">TXT</span>
              <span class="text-highlighted break-all">{{ created.verification?.txtRecord || "…" }}</span>
              <UButton
                v-if="created.verification"
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-copy"
                aria-label="Copy TXT record name"
                @click="copy('TXT record', created.verification!.txtRecord)"
              />
            </div>
            <div class="flex items-center gap-2">
              <span class="text-dimmed w-14 shrink-0">value</span>
              <span class="text-toned break-all">{{ created.verification?.txtValue || "…" }}</span>
              <UButton
                v-if="created.verification"
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-copy"
                aria-label="Copy TXT value"
                @click="copy('TXT value', created.verification!.txtValue)"
              />
            </div>
            <div v-if="created.verification?.cnameTarget" class="flex items-center gap-2">
              <span class="text-dimmed w-14 shrink-0">CNAME</span>
              <span class="text-toned break-all">{{ created.verification.cnameTarget }}</span>
              <UButton
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-copy"
                aria-label="Copy CNAME target"
                @click="copy('CNAME target', created.verification!.cnameTarget!)"
              />
            </div>
          </div>
          <p v-if="!created.verification" class="text-xs text-muted">
            Waiting for the operator to compute the record…
          </p>
          <p class="flex items-center gap-2 text-sm">
            <StatusDot :tone="created.verified ? 'success' : 'warning'" :pulse="!created.verified" />
            <span v-if="created.verified" class="text-success">Verified — the platform is taking it from here.</span>
            <span v-else class="text-muted">Waiting for the record… propagation can take a few minutes.</span>
          </p>
        </div>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <template v-if="!created">
            <UButton color="neutral" variant="ghost" @click="adding = false">Cancel</UButton>
            <UButton :disabled="!hostname" :loading="saving" icon="i-lucide-globe" @click="attach">
              Attach domain
            </UButton>
          </template>
          <UButton v-else color="neutral" variant="subtle" @click="adding = false">
            {{ created.verified ? "Done" : "I'll create the record — close" }}
          </UButton>
        </div>
      </template>
    </UModal>

    <!-- Detach confirmation. -->
    <UModal
      :open="removing !== null"
      :title="`Detach ${removing?.hostname}?`"
      description="The hostname comes off the gateway and its certificate is deleted. The DNS records in your zone are yours; the platform never touches them."
      @update:open="(open: boolean) => { if (!open) removing = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="removing = null">Cancel</UButton>
          <UButton color="error" :loading="deleting" icon="i-lucide-trash-2" @click="detach">
            Detach {{ removing?.hostname }}
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
