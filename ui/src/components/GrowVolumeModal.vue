<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type PlatformVolume } from "../lib/api";
import { formatBytes, parseQuantity } from "../lib/format";

// Growing one of the platform's own volumes.
//
// This is the lever the storage screen was missing at exactly the moment
// somebody needs it: a volume past 85% full, and a finding whose text named no
// way to do anything about it. The declaration behind these volumes cannot be
// changed on a running install — that is what made this four commands against
// the cluster and #533 the issue — so the platform does it instead: it expands
// the volume, rewrites the declaration behind it, and reports the outcome on
// the row this modal opened from.
//
// Nothing here can shrink a volume. Shrinking means replacing it and restoring
// what was on it, which is a different operation with a different risk, and the
// API refuses it — so this refuses it in the same words rather than offering a
// field whose Save is always a 400.

const props = defineProps<{ volume: PlatformVolume }>();
const emit = defineEmits<{ grown: [] }>();

const toast = useToast();
const open = ref(false);
const size = ref("");
const confirmation = ref("");
const saving = ref(false);

/** How much bigger one step may make it. The API refuses more, for the reason
 * it says: growing a volume cannot be undone, and a step that large is more
 * likely a typed unit than a decision. */
const MAX_GROWTH_FACTOR = 8;

/** What it asks for now, which is the floor for anything typed here. */
const current = computed(() => props.volume.requested || props.volume.capacity || "");
const currentBytes = computed(() => parseQuantity(current.value));

/** The one refusal nothing on either side can work around. `expandable` is
 * absent, rather than false, when nobody could tell — which is a reason not to
 * promise, not a reason to refuse. */
const cannotExpand = computed(() => props.volume.expandable === false);

const typedBytes = computed(() => parseQuantity(size.value.trim()));
const refusal = computed(() => {
  if (!size.value.trim()) return "";
  if (typedBytes.value === undefined) return "A size is a number and a unit, like 80Gi.";
  if (currentBytes.value === undefined) return "";
  if (typedBytes.value <= currentBytes.value) {
    return `This volume already asks for ${current.value}, and a volume is only ever grown.`;
  }
  if (typedBytes.value > currentBytes.value * MAX_GROWTH_FACTOR) {
    return `That is more than ${MAX_GROWTH_FACTOR} times ${current.value}. Growing a volume cannot be undone, so a step this large is refused in case it is a typed unit rather than a decision — grow it in steps.`;
  }
  return "";
});

/** Typing the name, for the reason every destructive write on this platform
 * asks for it: this one takes the workload apart and puts it back, and the
 * volume it grows cannot be made smaller again afterwards. */
const confirmed = computed(() => confirmation.value.trim() === props.volume.name);
const ready = computed(
  () => Boolean(size.value.trim()) && !refusal.value && !cannotExpand.value && confirmed.value,
);

watch(open, (value) => {
  if (!value) return;
  size.value = "";
  confirmation.value = "";
});

async function grow() {
  if (!ready.value || saving.value) return;
  saving.value = true;
  try {
    const accepted = await api.resizePlatformVolume(props.volume.name, size.value.trim());
    toast.add({
      title: `${accepted.claim} is growing to ${accepted.desired}`,
      description: accepted.message,
      color: "success",
      icon: "i-lucide-hard-drive",
    });
    open.value = false;
    emit("grown");
  } catch (err) {
    toast.add({
      title: "Growing the volume was refused",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    saving.value = false;
  }
}
</script>

<template>
  <UModal
    v-model:open="open"
    title="Grow this volume"
    description="The platform expands the volume and rewrites the declaration behind it, then waits. Nothing on the volume is touched and what is running on it keeps running."
  >
    <slot>
      <UButton icon="i-lucide-move-diagonal" color="neutral" variant="ghost" size="xs">Grow</UButton>
    </slot>

    <template #body>
      <div class="space-y-4">
        <div class="grid grid-cols-2 gap-3 text-xs">
          <div>
            <p class="text-[11px] text-muted">Volume</p>
            <p class="font-mono text-toned break-all">{{ volume.name }}</p>
          </div>
          <div>
            <p class="text-[11px] text-muted">Asks for now</p>
            <p class="font-mono text-toned">
              {{ current || "—"
              }}<span v-if="currentBytes !== undefined" class="text-dimmed"> ({{ formatBytes(currentBytes) }})</span>
            </p>
          </div>
        </div>

        <UAlert
          v-if="cannotExpand"
          color="warning"
          variant="soft"
          icon="i-lucide-triangle-alert"
          title="This volume cannot be grown"
          :description="`${volume.storageClass || 'The storage behind it'} does not allow expansion, and expansion is what growing a bound volume is. It can only be replaced, which is a restore rather than a resize.`"
        />

        <form v-else class="space-y-4" @submit.prevent="grow">
          <UFormField
            label="New size"
            help="A number and a unit — 80Gi, 1Ti. Larger than what it asks for now; volumes are grown and never shrunk."
            :error="refusal || undefined"
            required
          >
            <UInput v-model="size" placeholder="80Gi" class="w-full font-mono" autofocus />
          </UFormField>

          <UFormField
            label="Type the volume's name to confirm"
            help="Growing this one replaces the workload's declaration and cannot be undone: a smaller volume afterwards is a new volume and a restore."
            required
          >
            <UInput v-model="confirmation" :placeholder="volume.name" class="w-full font-mono" />
          </UFormField>

          <p class="text-[11px] text-dimmed leading-relaxed">
            The same size is also set by the chart, and neither overrules the other: the platform grows to the largest
            size anybody has asked for. Carrying this number back into the chart's values afterwards is what makes the
            two agree, and is what keeps a later rollback rendering the size that is actually there.
          </p>
        </form>
      </div>
    </template>

    <template v-if="!cannotExpand" #footer>
      <div class="flex justify-end gap-2 w-full">
        <UButton color="neutral" variant="ghost" @click="open = false">Cancel</UButton>
        <UButton :disabled="!ready" :loading="saving" @click="grow">Grow</UButton>
      </div>
    </template>
  </UModal>
</template>
