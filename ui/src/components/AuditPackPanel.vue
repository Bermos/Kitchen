<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { api, downloadAuditPack, type AuditPack, type Project } from "../lib/api";

// The evidence export.
//
// This is the screen the whole compliance suite has been building towards, and
// the design brief for it is one sentence: an auditor presses a button. Not a
// query language, not a ticket to the platform team, not four systems
// reconciled by hand — a project, a window, and a file.
//
// So the panel is deliberately small. Two selections and one button produce
// the pack; what comes back is shown as a summary rather than as JSON, because
// the person taking it is frequently not an engineer and the JSON is for the
// machine that reads it afterwards. Three things are shown before anything is
// saved, because each of them changes what the document is worth: whether it
// is signed, whether the window is fully covered by what the platform still
// holds, and how much is actually in it.
//
// Nothing here interprets the pack. The counts and the three warnings are read
// off fields the document carries for exactly this purpose; everything else
// stays in the file, which is where an examiner reads it.

const projects = ref<Project[]>([]);
const project = ref("");
const from = ref(defaultFrom());
const to = ref(defaultTo());

const pack = ref<AuditPack | null>(null);
const digest = ref("");
const bytes = ref(0);
const files = ref<{ pack?: Blob; packName?: string }>({});
const exporting = ref(false);
const error = ref("");

onMounted(async () => {
  try {
    projects.value = await api.projects();
    project.value = projects.value[0]?.name ?? "";
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  }
});

/** The default window is the last full quarter, because that is what an
 *  evidence request almost always asks for and because both ends of it are in
 *  the past — a window that ended "now" could not be reproduced, and the API
 *  refuses one. */
function quarterStart(at: Date): Date {
  return new Date(Date.UTC(at.getUTCFullYear(), Math.floor(at.getUTCMonth() / 3) * 3, 1));
}
function defaultTo(): string {
  return quarterStart(new Date()).toISOString().slice(0, 10);
}
function defaultFrom(): string {
  const current = quarterStart(new Date());
  return new Date(Date.UTC(current.getUTCFullYear(), current.getUTCMonth() - 3, 1))
    .toISOString()
    .slice(0, 10);
}

/** A date input gives a day; the API wants an instant. Midnight UTC is what a
 *  quarter's boundaries actually are. */
function instant(day: string): string {
  return `${day}T00:00:00Z`;
}

const projectOptions = computed(() => projects.value.map((p) => ({ label: p.name, value: p.name })));

const ready = computed(() => project.value !== "" && from.value !== "" && to.value !== "" && from.value < to.value);

const counts = computed(() => {
  const taken = pack.value;
  if (!taken) return [];
  return [
    { label: "Environments", value: taken.inventory.environments.length },
    { label: "Releases", value: taken.inventory.releases.length },
    { label: "Resource claims", value: taken.inventory.claims.length },
    { label: "Changes", value: taken.changeLog.length },
    { label: "Promotions", value: taken.promotions.length },
    { label: "Policy decisions", value: taken.decisions.items.length },
    { label: "Artifacts", value: taken.attestations.length },
    { label: "Exceptions", value: taken.exceptions.length },
    { label: "Re-evaluations", value: taken.drift.history.length },
    { label: "Audit records", value: taken.auditLog.items.length },
    { label: "Privileged of those", value: taken.auditLog.privileged },
    { label: "Signed statements", value: taken.signedRecords.items.length },
  ];
});

async function exportPack() {
  if (!ready.value) return;
  exporting.value = true;
  error.value = "";
  pack.value = null;
  try {
    const answer = await downloadAuditPack(
      project.value,
      { from: instant(from.value), to: instant(to.value) },
      "json",
    );
    const text = await answer.blob.text();
    pack.value = JSON.parse(text) as AuditPack;
    digest.value = answer.digest;
    bytes.value = answer.blob.size;
    files.value = { pack: answer.blob, packName: answer.filename };
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    exporting.value = false;
  }
}

function save(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  anchor.click();
  URL.revokeObjectURL(url);
}

/** The pack itself is already in hand — it was fetched to build the summary —
 *  so saving it costs no second request. The signature and the printable page
 *  are fetched when they are asked for. */
function savePack() {
  if (files.value.pack && files.value.packName) save(files.value.pack, files.value.packName);
}

const fetching = ref("");
async function saveOther(format: "dsse" | "html") {
  fetching.value = format;
  error.value = "";
  try {
    const answer = await downloadAuditPack(
      project.value,
      { from: instant(from.value), to: instant(to.value) },
      format,
    );
    save(answer.blob, answer.filename);
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    fetching.value = "";
  }
}

function readableSize(size: number): string {
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${(size / (1024 * 1024)).toFixed(1)} MB`;
}
</script>

<template>
  <div class="space-y-3">
    <div class="max-w-3xl">
      <p class="text-sm text-highlighted font-medium">Evidence export</p>
      <p class="text-xs text-muted mt-0.5">
        One project's whole compliance answer for one window, as a signed document: the inventory, the change
        log with the author and the approvers of every release, the promotions and the decisions behind them
        with the inputs they can be replayed from, the evidence attached to each artifact, the break-glass
        exceptions, the recertification cycles, what is running that no longer meets its bar, the audit log's
        own slice, and every signed statement carried whole. Assembling this by hand across four systems is
        the work this replaces.
      </p>
    </div>

    <div class="flex items-end gap-2 flex-wrap">
      <div>
        <label class="block text-xs text-muted mb-1" for="audit-pack-project">Project</label>
        <USelect id="audit-pack-project" v-model="project" :items="projectOptions" size="xs" class="w-56" />
      </div>
      <div>
        <label class="block text-xs text-muted mb-1" for="audit-pack-from">From (inclusive)</label>
        <UInput id="audit-pack-from" v-model="from" type="date" size="xs" class="w-40" />
      </div>
      <div>
        <label class="block text-xs text-muted mb-1" for="audit-pack-to">To (exclusive)</label>
        <UInput id="audit-pack-to" v-model="to" type="date" size="xs" class="w-40" />
      </div>
      <UButton
        icon="i-lucide-file-archive"
        color="primary"
        size="xs"
        :loading="exporting"
        :disabled="!ready"
        @click="exportPack"
      >
        Export
      </UButton>
    </div>
    <p class="text-[11px] text-dimmed">
      Both ends are required and the window is half-open — the start is in it, the end is not — so consecutive
      quarters tile without counting anything twice. A pack that ended "now" could not be reproduced, which is
      why there is no such option: two exports of the same window are the same bytes unless the evidence
      changed.
    </p>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <div v-if="pack" class="rounded-md border border-default p-3 space-y-3">
      <div class="flex items-start justify-between gap-4 flex-wrap">
        <div>
          <p class="text-sm text-highlighted font-medium">
            {{ pack.project }} · {{ pack.range.from.slice(0, 10) }} → {{ pack.range.to.slice(0, 10) }}
          </p>
          <p class="text-[11px] text-dimmed font-mono break-all mt-0.5">{{ digest }}</p>
          <p class="text-[11px] text-dimmed">{{ readableSize(bytes) }}</p>
        </div>
        <div class="flex items-center gap-2">
          <UButton icon="i-lucide-download" color="primary" variant="subtle" size="xs" @click="savePack">
            Pack (JSON)
          </UButton>
          <UButton
            v-if="pack.verification.signed"
            icon="i-lucide-signature"
            color="neutral"
            variant="subtle"
            size="xs"
            :loading="fetching === 'dsse'"
            @click="saveOther('dsse')"
          >
            Signature
          </UButton>
          <UButton
            icon="i-lucide-printer"
            color="neutral"
            variant="subtle"
            size="xs"
            :loading="fetching === 'html'"
            @click="saveOther('html')"
          >
            Printable page
          </UButton>
        </div>
      </div>

      <!-- The three things that change what the document is worth, said before
           anybody saves it rather than left inside the file to be found. -->
      <UAlert
        v-if="!pack.verification.signed"
        color="warning"
        variant="soft"
        icon="i-lucide-shield-off"
        title="This pack is unsigned"
        :description="pack.verification.message"
      />
      <UAlert
        v-if="pack.retention.truncated"
        color="warning"
        variant="soft"
        icon="i-lucide-history"
        title="Retention has truncated this window"
        :description="pack.retention.message"
      />
      <UAlert
        v-if="pack.decisions.truncated || pack.auditLog.truncated"
        color="warning"
        variant="soft"
        icon="i-lucide-scissors"
        title="A section reached its limit"
        :description="pack.decisions.message || pack.auditLog.message"
      />
      <UAlert
        v-if="!pack.platform.rescanning"
        color="warning"
        variant="soft"
        icon="i-lucide-radar"
        title="Continuous re-evaluation is not running"
        :description="
          pack.platform.rescanMessage ||
          'Nothing in the drift section of this pack is a statement about today.'
        "
      />

      <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-x-4 gap-y-1">
        <div v-for="row in counts" :key="row.label" class="flex items-baseline justify-between gap-2">
          <span class="text-xs text-muted">{{ row.label }}</span>
          <span class="text-xs font-mono text-highlighted">{{ row.value }}</span>
        </div>
      </div>

      <div class="border-t border-default pt-3">
        <p class="text-xs text-highlighted font-medium">Checking it without Kitchen</p>
        <p class="text-[11px] text-dimmed mt-0.5">{{ pack.verification.warning }}</p>
        <ol class="mt-1 space-y-1">
          <li v-for="step in pack.verification.procedure" :key="step" class="text-[11px] font-mono text-toned break-all">
            {{ step }}
          </li>
        </ol>
      </div>
    </div>
  </div>
</template>
