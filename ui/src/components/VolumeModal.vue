<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api } from "../lib/api";

// Writing the one object a bound volume claim needs and nothing on the
// platform made: storage that existed before this installation did — the NFS
// export on the NAS, the volume a storage appliance's driver hands out.
//
// Two shapes, because the spike found two. There is deliberately no third for
// a directory on one machine: that ties whatever mounts it to that machine,
// and the API refuses it with the reason.
//
// Nothing here is a credential, and that is enforced rather than assumed. The
// API refuses a reference to a stored secret and refuses a driver attribute
// whose name reads like a password, because this object is readable by
// anything that can read storage at all.

const emit = defineEmits<{ saved: [] }>();

const toast = useToast();
const open = ref(false);

const kinds = [
  { label: "NFS export — a share on a file server", value: "nfs" },
  { label: "CSI volume — one a storage driver already knows about", value: "csi" },
];

const name = ref("");
const kind = ref("nfs");
const capacity = ref("");
const modes = ref<string[]>(["ReadWriteMany"]);

const server = ref("");
const exportPath = ref("");
const driver = ref("");
const handle = ref("");
const fsType = ref("");
const readOnly = ref(false);

// The driver's own configuration, as it hands it out. Names that read like a
// credential are refused by the API — these are written in the clear.
const attributes = ref<{ key: string; value: string }[]>([]);

const isNFS = computed(() => kind.value === "nfs");

/** What the storage can do. It is not what any one project may do with it:
 * a claim declares its own mode, and that is what decides who may write. */
const accessModes = [
  { value: "ReadOnlyMany", label: "Read by many", help: "Any number of projects and copies may read it" },
  { value: "ReadWriteOnce", label: "Written by one", help: "Attaches to one copy of one process at a time" },
  { value: "ReadWriteMany", label: "Written by many", help: "A shared filesystem — NFS and SMB do this" },
];

watch(open, (value) => {
  if (!value) return;
  name.value = "";
  kind.value = "nfs";
  capacity.value = "";
  modes.value = ["ReadWriteMany"];
  server.value = "";
  exportPath.value = "";
  driver.value = "";
  handle.value = "";
  fsType.value = "";
  readOnly.value = false;
  attributes.value = [];
});

function toggleMode(mode: string, on: boolean) {
  modes.value = on ? [...new Set([...modes.value, mode])] : modes.value.filter((m) => m !== mode);
}

const ready = computed(() => {
  if (!name.value.trim() || !capacity.value.trim() || !modes.value.length) return false;
  return isNFS.value
    ? Boolean(server.value.trim() && exportPath.value.trim())
    : Boolean(driver.value.trim() && handle.value.trim());
});

const saving = ref(false);
async function save() {
  if (!ready.value || saving.value) return;
  saving.value = true;
  try {
    const written = await api.createPlatformWrittenVolume({
      name: name.value.trim(),
      capacity: capacity.value.trim(),
      accessModes: modes.value,
      ...(isNFS.value
        ? { nfs: { server: server.value.trim(), path: exportPath.value.trim(), readOnly: readOnly.value } }
        : {
            csi: {
              driver: driver.value.trim(),
              volumeHandle: handle.value.trim(),
              ...(fsType.value.trim() ? { fsType: fsType.value.trim() } : {}),
              readOnly: readOnly.value,
              ...(attributes.value.some((a) => a.key.trim())
                ? {
                    volumeAttributes: Object.fromEntries(
                      attributes.value.filter((a) => a.key.trim()).map((a) => [a.key.trim(), a.value]),
                    ),
                  }
                : {}),
            },
          }),
    });
    toast.add({ title: `Volume ${written.name} written`, color: "success", icon: "i-lucide-hard-drive" });
    open.value = false;
    emit("saved");
  } catch (err) {
    toast.add({
      title: "Writing the volume failed",
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
    title="New volume"
    description="Point the platform at storage that already exists, so that a project can mount it. Nothing is formatted, nothing is created on the server, and the data is never touched."
  >
    <slot>
      <UButton icon="i-lucide-plus" size="sm">New volume</UButton>
    </slot>

    <template #body>
      <form class="space-y-4" @submit.prevent="save">
        <div class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Name" help="What a claim names to mount it. Lowercase letters, digits and dashes." required>
            <UInput v-model="name" placeholder="nas-media" class="w-full font-mono" autofocus />
          </UFormField>
          <UFormField label="Kind" required>
            <USelect v-model="kind" :items="kinds" class="w-full" />
          </UFormField>
        </div>

        <template v-if="isNFS">
          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Server" help="The host serving the export." required>
              <UInput v-model="server" placeholder="nas.lan" class="w-full font-mono" />
            </UFormField>
            <UFormField label="Export" help="The path as the server exports it." required>
              <UInput v-model="exportPath" placeholder="/export/media" class="w-full font-mono" />
            </UFormField>
          </div>
        </template>

        <template v-else>
          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Driver" help="The name the storage driver registers under." required>
              <UInput v-model="driver" placeholder="csi.truenas.net" class="w-full font-mono" />
            </UFormField>
            <UFormField label="Volume handle" help="The id the driver knows this volume by." required>
              <UInput v-model="handle" placeholder="tank/photos" class="w-full font-mono" />
            </UFormField>
          </div>
          <UFormField label="Filesystem" help="What the driver should mount it as. Empty leaves it to the driver.">
            <UInput v-model="fsType" placeholder="ext4" class="w-full font-mono" />
          </UFormField>

          <!-- The driver's own configuration. It is written in the clear and
               read back by every listing, so the API refuses any key that
               reads like a credential — a driver that cannot mount without a
               secret is not expressible here. -->
          <UFormField
            label="Driver settings"
            help="Whatever the driver asks for. They are stored in the clear, so nothing that is a password may go here."
          >
            <div class="space-y-2">
              <div v-for="(attribute, index) in attributes" :key="index" class="flex gap-2">
                <UInput v-model="attribute.key" placeholder="share" class="flex-1 font-mono" />
                <UInput v-model="attribute.value" placeholder="photos" class="flex-1 font-mono" />
                <UButton
                  color="neutral"
                  variant="ghost"
                  size="xs"
                  icon="i-lucide-x"
                  aria-label="Remove setting"
                  @click="attributes.splice(index, 1)"
                />
              </div>
              <UButton
                color="neutral"
                variant="subtle"
                size="xs"
                icon="i-lucide-plus"
                @click="attributes.push({ key: '', value: '' })"
              >
                Add a setting
              </UButton>
            </div>
          </UFormField>
        </template>

        <div class="grid gap-4 sm:grid-cols-2">
          <UFormField
            label="Capacity"
            help="How much the storage offers. It is what a claim asks for; nothing enforces it against the server."
            required
          >
            <UInput v-model="capacity" placeholder="12Ti" class="w-full font-mono" />
          </UFormField>
          <UFormField label="Mount read-only" help="Refuses every write, whatever a project asks for.">
            <USwitch v-model="readOnly" />
          </UFormField>
        </div>

        <UFormField
          label="What the storage can do"
          help="Not what any one project may do with it — a claim declares its own, and that is what decides who may write."
          required
        >
          <div class="space-y-2">
            <div v-for="mode in accessModes" :key="mode.value" class="flex items-start gap-2">
              <UCheckbox
                :model-value="modes.includes(mode.value)"
                :label="mode.label"
                :description="mode.help"
                @update:model-value="(on: boolean | 'indeterminate') => toggleMode(mode.value, on === true)"
              />
            </div>
          </div>
        </UFormField>

        <UAlert
          color="neutral"
          variant="soft"
          icon="i-lucide-shield"
          title="Nothing here can delete data"
          description="Volumes written from this screen always retain their data. Removing one removes the platform's record of it; the export, the share and every byte on it stay exactly where they are."
        />
      </form>
    </template>

    <template #footer>
      <div class="flex justify-end gap-2 w-full">
        <UButton color="neutral" variant="subtle" @click="open = false">Cancel</UButton>
        <UButton :disabled="!ready" :loading="saving" icon="i-lucide-hard-drive" @click="save">Write volume</UButton>
      </div>
    </template>
  </UModal>
</template>
