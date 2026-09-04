<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, APIError, downloadBackup, type BackupDestinationWrite, type BackupHolds } from "../lib/api";
import { useAsync } from "../lib/useAsync";
import { formatBytes, timeAgo } from "../lib/format";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";
import StatusDot from "../components/StatusDot.vue";

// Backing the platform up: what an archive would carry, what it deliberately
// would not, and the button that takes one.
//
// There is no restore control on this screen, and that is the design rather
// than a gap. A restore happens into a cluster whose accounts database is gone
// — and the credentials to log in here are inside the archive, so there is
// nobody left to press anything. Restore is a Job the chart renders, which
// puts it in the same category as installing the chart and following the
// bootstrap link. docs/BACKUP.md is the procedure.
//
// The exclusions are served by the API rather than written here, so this screen
// and the archive's own manifest cannot come to disagree about what is missing.

const { data, error, loading, refresh } = useAsync(() => api.backup());

const taking = ref(false);
const failure = ref("");
const takenAt = ref("");

const resources = computed(() => {
  const counts = data.value?.resources ?? {};
  return Object.entries(counts)
    .filter(([, count]) => count > 0)
    .sort(([a], [b]) => a.localeCompare(b));
});

const objects = computed(() =>
  Object.values(data.value?.resources ?? {}).reduce((total, count) => total + count, 0),
);

async function take() {
  taking.value = true;
  failure.value = "";
  try {
    const { blob, filename } = await downloadBackup();
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = filename;
    link.click();
    URL.revokeObjectURL(url);
    takenAt.value = filename;
    await refresh();
  } catch (err) {
    failure.value = err instanceof APIError ? err.message : String(err);
  } finally {
    taking.value = false;
  }
}

// The scheduled half.
//
// `lastSuccess` is the field this section exists for. Everything else here
// makes backups happen; that one is the only thing that makes their absence
// visible, and six weeks of no archive that nobody noticed is how a backup
// system actually fails.

const schedule = computed(() => data.value?.schedule);
const configured = computed(() => Boolean(schedule.value?.schedule && schedule.value?.destination));

/** The schedule's own state, as one dot. It follows the platform's own
 * `BackupReady` condition rather than a second opinion derived here, so this
 * screen and the platform's status cannot come to disagree. */
const tone = computed<"success" | "warning" | "error" | "neutral">(() => {
  const state = schedule.value;
  if (!state) return "neutral";
  if (!configured.value) return "warning";
  if (!state.ready) return "error";
  if (state.suspended) return "warning";
  return "success";
});

// The schedule form. It mirrors the served values whenever they change, so a
// refresh after a save does not leave the fields showing what was typed.
const cron = ref("");
const suspended = ref(false);
const keepLast = ref(0);
const keepDays = ref(0);
const savingSchedule = ref(false);
const scheduleError = ref("");

watch(
  schedule,
  (state) => {
    cron.value = state?.schedule ?? "";
    suspended.value = state?.suspended ?? false;
    keepLast.value = state?.keepLast ?? 0;
    keepDays.value = state?.keepDays ?? 0;
  },
  { immediate: true },
);

const scheduleDirty = computed(() => {
  const state = schedule.value;
  if (!state) return false;
  return (
    cron.value.trim() !== (state.schedule ?? "") ||
    suspended.value !== state.suspended ||
    keepLast.value !== (state.keepLast ?? 0) ||
    keepDays.value !== (state.keepDays ?? 0)
  );
});

async function saveSchedule() {
  savingSchedule.value = true;
  scheduleError.value = "";
  try {
    await api.updateSettings({
      backupSchedule: cron.value.trim(),
      backupSuspend: suspended.value,
      backupKeepLast: keepLast.value,
      backupKeepDays: keepDays.value,
    });
    await refresh();
  } catch (err) {
    scheduleError.value = err instanceof APIError ? err.message : String(err);
  } finally {
    savingSchedule.value = false;
  }
}

// The destination form.
//
// The key fields start empty and stay empty however many times this screen is
// opened, because the API never reads a credential back. Leaving them empty
// means "leave the stored credential alone", which is what makes editing a
// destination's prefix possible without retyping a key nobody can see.
const bucket = ref("");
const prefix = ref("");
const region = ref("");
const endpoint = ref("");
const pathStyle = ref(false);
const encryption = ref("");
const kmsKeyId = ref("");
const accessKeyId = ref("");
const secretAccessKey = ref("");
const ambient = ref(false);
const savingDestination = ref(false);
const destinationError = ref("");
const removing = ref(false);

const encryptions = [
  { label: "Whatever the bucket does by default", value: "" },
  { label: "AES256", value: "AES256" },
  { label: "aws:kms", value: "aws:kms" },
];

watch(
  () => schedule.value?.destination,
  (target) => {
    bucket.value = target?.bucket ?? "";
    prefix.value = target?.prefix ?? "";
    region.value = target?.region ?? "";
    endpoint.value = target?.endpoint ?? "";
    pathStyle.value = target?.forcePathStyle ?? false;
    encryption.value = target?.serverSideEncryption ?? "";
    kmsKeyId.value = target?.kmsKeyId ?? "";
    accessKeyId.value = "";
    secretAccessKey.value = "";
    ambient.value = false;
  },
  { immediate: true },
);

async function saveDestination() {
  savingDestination.value = true;
  destinationError.value = "";
  const body: BackupDestinationWrite = {
    type: "s3",
    s3: {
      bucket: bucket.value.trim(),
      prefix: prefix.value.trim(),
      region: region.value.trim(),
      endpoint: endpoint.value.trim(),
      forcePathStyle: pathStyle.value,
      serverSideEncryption: encryption.value,
      kmsKeyId: kmsKeyId.value.trim(),
    },
  };
  if (ambient.value) body.s3.ambientCredentials = true;
  else if (accessKeyId.value || secretAccessKey.value) {
    body.s3.accessKeyId = accessKeyId.value.trim();
    body.s3.secretAccessKey = secretAccessKey.value.trim();
  }
  try {
    await api.setBackupDestination(body);
    await refresh();
    await loadHolds();
  } catch (err) {
    destinationError.value = err instanceof APIError ? err.message : String(err);
  } finally {
    savingDestination.value = false;
  }
}

async function removeDestination() {
  removing.value = true;
  destinationError.value = "";
  try {
    await api.removeBackupDestination();
    holds.value = undefined;
    await refresh();
  } catch (err) {
    destinationError.value = err instanceof APIError ? err.message : String(err);
  } finally {
    removing.value = false;
  }
}

// What the destination actually holds. It is read from the destination rather
// than from the platform's own status: the status says what the last run
// believed it did, and this says what is there now — the only half a recovery
// can use.
const holds = ref<BackupHolds | undefined>(undefined);
const listing = ref(false);
const listError = ref("");
const running = ref(false);
const startedJob = ref("");

async function loadHolds() {
  if (!schedule.value?.destination) return;
  listing.value = true;
  listError.value = "";
  try {
    holds.value = await api.backupHolds();
  } catch (err) {
    listError.value = err instanceof APIError ? err.message : String(err);
  } finally {
    listing.value = false;
  }
}

watch(
  () => schedule.value?.destination?.described,
  (described) => {
    if (described && !holds.value) void loadHolds();
  },
);

async function runNow() {
  running.value = true;
  listError.value = "";
  startedJob.value = "";
  try {
    const started = await api.runBackup();
    startedJob.value = started.job;
  } catch (err) {
    listError.value = err instanceof APIError ? err.message : String(err);
  } finally {
    running.value = false;
  }
}
</script>

<template>
  <div class="space-y-6">
    <PageHeader title="Backup" :breadcrumb="[{ label: 'Platform', to: '/platform' }, { label: 'Backup' }]">
      <template #description>
        One archive: every Kitchen object, every credential in the platform namespace, and the identity provider's
        database.
      </template>
      <template #actions>
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="loading"
          aria-label="Refresh"
          @click="refresh"
        />
        <UButton
          icon="i-lucide-play"
          color="neutral"
          variant="subtle"
          size="sm"
          :loading="running"
          :disabled="!configured"
          @click="runNow"
        >
          Run now
        </UButton>
        <UButton icon="i-lucide-download" color="primary" size="sm" :loading="taking" :disabled="!data" @click="take">
          Take a backup
        </UButton>
      </template>
    </PageHeader>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <UAlert v-if="failure" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="failure" />

    <!-- The archive is a credential in its own right, and saying so once here
         is cheaper than explaining it after somebody has left one in a shared
         drive. -->
    <UAlert
      v-if="takenAt"
      color="warning"
      variant="soft"
      icon="i-lucide-shield-alert"
      title="The archive is a credential"
      :description="`${takenAt} holds every secret this platform has, in the clear. Keep it where you would keep the cluster's root credentials, and off the cluster it came from.`"
    />

    <template v-if="data">
      <div class="grid grid-cols-2 sm:grid-cols-4 gap-3">
        <div class="rounded-md border border-default px-4 py-3">
          <p class="text-xs text-muted">Objects</p>
          <p class="text-lg font-semibold text-highlighted tabular-nums mt-1">{{ objects }}</p>
          <p class="text-[11px] text-dimmed mt-0.5">projects, releases, environments</p>
        </div>
        <div class="rounded-md border border-default px-4 py-3">
          <p class="text-xs text-muted">Secrets</p>
          <p class="text-lg font-semibold text-highlighted tabular-nums mt-1">{{ data.secrets }}</p>
          <p class="text-[11px] text-dimmed mt-0.5">in the platform namespace</p>
        </div>
        <div
          class="rounded-md border px-4 py-3"
          :class="data.accounts.available ? 'border-default' : 'border-warning/40 bg-warning/5'"
        >
          <p class="text-xs text-muted">Accounts</p>
          <p
            class="text-lg font-semibold tabular-nums mt-1"
            :class="data.accounts.available ? 'text-highlighted' : 'text-warning'"
          >
            {{ data.accounts.available ? data.accounts.database : "not included" }}
          </p>
          <p class="text-[11px] text-dimmed mt-0.5">the identity provider's database</p>
        </div>
        <div class="rounded-md border border-default px-4 py-3">
          <p class="text-xs text-muted">Release</p>
          <p class="text-lg font-semibold text-highlighted tabular-nums mt-1">{{ data.platformVersion }}</p>
          <p class="text-[11px] text-dimmed mt-0.5">what it restores into</p>
        </div>
      </div>

      <!-- An installation with no identity provider has no accounts to take,
           which is not a fault; one whose database cannot be reached is. The
           message is the API's, and it says which. -->
      <UAlert
        v-if="!data.accounts.available"
        color="warning"
        variant="soft"
        icon="i-lucide-database"
        title="This archive would carry no accounts"
        :description="data.accounts.message"
      />

      <!-- The scheduled half, above what an archive would carry, because "when
           did one last work" is the question this screen is opened with and
           "what would one hold" is the question it is read with. -->
      <PageSection
        title="Schedule"
        description="A five-field cron expression in UTC, and somewhere off this cluster to write to. The operator runs the same exporter the button above does, so a scheduled archive and a manual one are the same file."
      >
        <template #actions>
          <UButton
            v-if="schedule?.destination"
            icon="i-lucide-refresh-cw"
            color="neutral"
            variant="ghost"
            size="xs"
            :loading="listing"
            @click="loadHolds"
          >
            Re-read the destination
          </UButton>
        </template>

        <div class="rounded-md border border-default px-4 py-3 space-y-4">
          <p class="text-xs flex items-start gap-2 text-toned">
            <StatusDot :tone="tone" class="mt-1 shrink-0" />
            <span>{{ schedule?.message || "This platform has no scheduled backup." }}</span>
          </p>

          <div class="grid grid-cols-2 sm:grid-cols-4 gap-3">
            <div>
              <p class="text-xs text-muted">Runs</p>
              <p class="font-mono text-sm text-highlighted mt-1">{{ schedule?.schedule || "never" }}</p>
              <p class="text-[11px] text-dimmed mt-0.5">UTC, always</p>
            </div>
            <div>
              <p class="text-xs text-muted">Writes to</p>
              <p class="font-mono text-sm text-highlighted mt-1 truncate" :title="schedule?.destination?.described">
                {{ schedule?.destination?.described || "nowhere" }}
              </p>
              <p class="text-[11px] text-dimmed mt-0.5">
                {{ schedule?.destination ? `${schedule.destination.credential} credential` : "off this cluster" }}
              </p>
            </div>
            <div>
              <p class="text-xs text-muted">Last success</p>
              <p class="text-sm mt-1" :class="schedule?.lastSuccess ? 'text-highlighted' : 'text-warning'">
                {{ schedule?.lastSuccess ? timeAgo(schedule.lastSuccess) : "never" }}
              </p>
              <p class="text-[11px] text-dimmed mt-0.5">
                {{ schedule?.lastSuccessBytes ? formatBytes(schedule.lastSuccessBytes) : "the number to watch" }}
              </p>
            </div>
            <div>
              <p class="text-xs text-muted">Last failure</p>
              <p class="text-sm mt-1" :class="schedule?.lastFailure ? 'text-error' : 'text-highlighted'">
                {{ schedule?.lastFailure ? timeAgo(schedule.lastFailure) : "none" }}
              </p>
              <p class="text-[11px] text-dimmed mt-0.5">{{ schedule?.archives ?? 0 }} archives kept</p>
            </div>
          </div>

          <UAlert
            v-if="startedJob"
            color="neutral"
            variant="soft"
            icon="i-lucide-play"
            title="A backup is running"
            :description="`It writes to ${schedule?.destination?.described}. Re-read the destination in a minute to see what arrived.`"
          />
          <UAlert
            v-if="scheduleError"
            color="error"
            variant="soft"
            icon="i-lucide-triangle-alert"
            :title="scheduleError"
          />

          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField
              label="Schedule"
              help="Five fields, in UTC. Pick a quiet hour: the accounts half is taken through the identity provider's database. Empty turns the scheduled backup off."
            >
              <UInput v-model="cron" placeholder="0 3 * * *" class="w-full font-mono" />
            </UFormField>
            <UFormField
              label="Suspended"
              help="Pauses the schedule without losing it — the answer to maintenance that is not deleting the configuration and hoping somebody puts it back."
            >
              <USwitch v-model="suspended" />
            </UFormField>
            <UFormField
              label="Keep the newest"
              help="How many archives survive a prune. 0 keeps every one, which is the safe default — an archive costs pennies."
            >
              <UInputNumber v-model="keepLast" :min="0" :max="3650" class="w-40" />
            </UFormField>
            <UFormField
              label="Keep for (days)"
              help="Archives older than this are pruned. 0 removes the bound. Both bounds apply where both are set."
            >
              <UInputNumber v-model="keepDays" :min="0" :max="3650" class="w-40" />
            </UFormField>
          </div>
          <p class="text-[11px] text-dimmed leading-relaxed">
            One run exports, uploads, reads the archive back to prove it arrived, and only then prunes. Pruning last is
            deliberate: a prune that ran first would delete last week&apos;s archive on the night this week&apos;s
            failed. Retention only ever touches keys named the way this platform names an archive, so a bucket you also
            keep other things in does not lose them — and it is not a safety property, because it deletes. Object Lock
            is the store&apos;s answer to that, and Kitchen does not manage it.
          </p>
          <div class="flex justify-end">
            <UButton :disabled="!scheduleDirty" :loading="savingSchedule" icon="i-lucide-save" @click="saveSchedule">
              Save the schedule
            </UButton>
          </div>
        </div>
      </PageSection>

      <PageSection
        title="Destination"
        description="Any S3-compatible store — AWS, MinIO, R2, Backblaze, Wasabi, Ceph, Garage — because the endpoint override makes those one path rather than six. There is deliberately no local destination: an archive on a volume here does not survive the loss of here."
      >
        <div class="rounded-md border border-default px-4 py-3 space-y-4">
          <UAlert
            color="warning"
            variant="soft"
            icon="i-lucide-shield-alert"
            title="This bucket becomes the platform's root credential store"
            description="An archive holds every secret this platform has, in the clear. Give it its own bucket, no public access, server-side encryption, and a key that can write and list and is not one of the keys the platform itself holds. Keep that key outside the platform too — it is inside the archive, and the archive is inside the bucket."
          />
          <UAlert
            v-if="destinationError"
            color="error"
            variant="soft"
            icon="i-lucide-triangle-alert"
            :title="destinationError"
          />

          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Bucket" help="Where archives are written. Its own bucket, ideally.">
              <UInput v-model="bucket" placeholder="kitchen-backups" class="w-full" />
            </UFormField>
            <UFormField label="Prefix" help="The key prefix inside it. Retention only ever looks under this.">
              <UInput v-model="prefix" placeholder="prod" class="w-full" />
            </UFormField>
            <UFormField
              label="Region"
              help="Wanted even by stores where it means nothing; us-east-1 is the usual answer for those."
            >
              <UInput v-model="region" placeholder="eu-central-1" class="w-full" />
            </UFormField>
            <UFormField label="Endpoint" help="Empty is AWS. Anything else — MinIO, R2, Garage — is named here.">
              <UInput v-model="endpoint" placeholder="https://minio.example.com" class="w-full" />
            </UFormField>
            <UFormField
              label="Path-style addressing"
              help="Address the bucket as endpoint/bucket. Needed by any store reached by IP address, or by a name with no wildcard certificate behind it."
            >
              <USwitch v-model="pathStyle" />
            </UFormField>
            <UFormField
              label="Server-side encryption"
              help="Ask the store to encrypt the archive at rest. It should be told to."
            >
              <USelect v-model="encryption" :items="encryptions" class="w-full" />
            </UFormField>
            <UFormField v-if="encryption === 'aws:kms'" label="KMS key" help="Which key encrypts it.">
              <UInput v-model="kmsKeyId" class="w-full" />
            </UFormField>
          </div>

          <div class="rounded-md border border-default px-4 py-3 space-y-4">
            <h3 class="text-xs font-medium text-highlighted">Credential</h3>
            <p class="text-[11px] text-dimmed leading-relaxed">
              Nothing here is ever read back, so these two fields are empty however many times this screen is opened —
              and leaving them empty means the stored key is left alone. Better still is no long-lived key at all: a
              destination on the ambient chain authenticates with the credential the platform already has from its
              environment, and there is then nothing to leak.
            </p>
            <UFormField
              label="Use the ambient credential chain"
              help="Deletes the key this platform is storing and authenticates with whatever the environment already provides."
            >
              <USwitch v-model="ambient" />
            </UFormField>
            <div v-if="!ambient" class="grid gap-4 sm:grid-cols-2">
              <UFormField
                label="Access key ID"
                :help="
                  schedule?.destination?.credential === 'stored'
                    ? 'A key is stored. Leave both empty to keep it.'
                    : 'Both fields, or neither.'
                "
              >
                <UInput v-model="accessKeyId" autocomplete="off" class="w-full" />
              </UFormField>
              <UFormField
                label="Secret access key"
                help="Written into a Secret this platform owns, and never served again."
              >
                <UInput v-model="secretAccessKey" type="password" autocomplete="off" class="w-full" />
              </UFormField>
            </div>
          </div>

          <div class="flex justify-end gap-2">
            <UButton
              v-if="schedule?.destination"
              color="error"
              variant="subtle"
              icon="i-lucide-trash-2"
              :loading="removing"
              @click="removeDestination"
            >
              Remove the destination
            </UButton>
            <UButton :disabled="!bucket" :loading="savingDestination" icon="i-lucide-save" @click="saveDestination">
              Save the destination
            </UButton>
          </div>
        </div>
      </PageSection>

      <PageSection
        v-if="schedule?.destination"
        title="What is at the destination"
        description="Read from the destination itself, not from this platform's belief about it. The status says what the last run thought it did; this says what a recovery would actually find."
      >
        <UAlert
          v-if="listError"
          color="error"
          variant="soft"
          icon="i-lucide-triangle-alert"
          :title="listError"
          class="mb-3"
        />
        <div class="rounded-md border border-default bg-muted overflow-x-auto">
          <table class="w-full min-w-[36rem] text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default">
                <th class="px-3 py-2 font-medium">Key</th>
                <th class="px-3 py-2 font-medium">Size</th>
                <th class="px-3 py-2 font-medium">Written</th>
                <th class="px-3 py-2 font-medium">What</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!holds?.objects?.length">
                <td colspan="4" class="px-3 py-8 text-center text-muted text-xs">
                  {{ listing ? "Reading the destination…" : "Nothing at the destination yet." }}
                </td>
              </tr>
              <tr v-for="object in holds?.objects ?? []" :key="object.key" class="border-b border-muted last:border-0">
                <td class="px-3 py-2 font-mono text-xs text-highlighted">{{ object.key }}</td>
                <td class="px-3 py-2 font-mono text-xs text-toned whitespace-nowrap">{{ formatBytes(object.size) }}</td>
                <td class="px-3 py-2 text-xs text-toned whitespace-nowrap">{{ timeAgo(object.modified) }}</td>
                <td class="px-3 py-2 text-xs" :class="object.archive ? 'text-toned' : 'text-muted'">
                  {{ object.archive ? "archive" : "not ours" }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-if="holds?.truncated" class="text-[11px] text-dimmed mt-2">
          The newest 200 only. The destination holds more.
        </p>
      </PageSection>

      <div class="grid gap-4 lg:grid-cols-2">
        <div>
          <h2 class="text-sm font-medium text-highlighted mb-2">What it carries</h2>
          <div class="rounded-md border border-default divide-y divide-muted">
            <div v-for="[kind, count] in resources" :key="kind" class="flex items-center justify-between px-4 py-2">
              <span class="text-xs text-toned">{{ kind }}</span>
              <span class="font-mono text-xs tabular-nums text-highlighted">{{ count }}</span>
            </div>
            <div v-if="!resources.length" class="px-4 py-6 text-center text-xs text-muted">
              This platform holds no objects yet.
            </div>
            <div class="flex items-center justify-between px-4 py-2">
              <span class="text-xs text-toned">secrets</span>
              <span class="font-mono text-xs tabular-nums text-highlighted">{{ data.secrets }}</span>
            </div>
          </div>
          <p class="text-[11px] text-dimmed mt-2 leading-relaxed">
            The credentials are the part that matters most and the part easiest to leave out. A restore without the
            Cloudflare token, the git app keys and the identity provider's own signing secret brings back a platform
            that cannot talk to anything.
          </p>
        </div>

        <div>
          <h2 class="text-sm font-medium text-highlighted mb-2">What it does not</h2>
          <ul class="rounded-md border border-default divide-y divide-muted">
            <li v-for="item in data.excluded" :key="item" class="px-4 py-2 text-xs text-muted leading-relaxed">
              {{ item }}
            </li>
          </ul>
        </div>
      </div>

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">Volume snapshots</h2>
        <div
          class="rounded-md border px-4 py-3"
          :class="data.snapshots.supported ? 'border-default' : 'border-default bg-elevated/30'"
        >
          <p class="text-xs flex items-start gap-2" :class="data.snapshots.supported ? 'text-toned' : 'text-muted'">
            <StatusDot :tone="data.snapshots.supported ? 'success' : 'neutral'" class="mt-1 shrink-0" />
            <span v-if="data.snapshots.supported">
              This cluster can snapshot volumes, through
              <span class="font-mono">{{ (data.snapshots.classes ?? []).join(", ") }}</span
              >. That is the cheap answer for the two volumes this archive cannot carry — the accounts database and the
              telemetry store — and it is an option rather than the plan: a snapshot lives on the same storage as the
              volume it copies.
            </span>
            <span v-else>{{ data.snapshots.message }}</span>
          </p>
        </div>
      </div>

      <div class="rounded-md border border-default px-4 py-3">
        <h2 class="text-sm font-medium text-highlighted mb-1">Restoring</h2>
        <p class="text-xs text-muted leading-relaxed">
          There is no restore button, and there cannot be one: a restore happens into a cluster whose accounts database
          is gone, so there is nobody left to log in with. The chart renders a Job for it instead — the same category as
          installing the chart and following the bootstrap link. An archive restores into the release that wrote it
          ({{ data.platformVersion }}); upgrade afterwards, not before. The procedure, and the CI job that runs it on
          every change, are in <span class="font-mono">docs/BACKUP.md</span>.
        </p>
      </div>
    </template>
    <p v-else-if="loading" class="text-xs text-muted">Loading…</p>
  </div>
</template>
