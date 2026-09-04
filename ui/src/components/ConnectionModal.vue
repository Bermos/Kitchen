<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type Connection, type ConnectionTestResult } from "../lib/api";
import { providerGuidance, testSummary, testTone } from "../lib/connectors";

// The create flow the connections page used to point at kubectl for — and the
// edit flow that rotates a credential. The credential goes to the operator and
// never comes back: editing always starts from blank credential fields, and
// leaving them blank keeps what is stored.
//
// Which token, allowed to do what, from where: the form answers all three
// before it asks (lib/connectors.ts), and "Test connection" runs the probe the
// ConnectionReconciler runs — storing nothing — so a token that is missing a
// permission is caught here rather than at the first failed webhook.

const props = defineProps<{
  /** When set, the modal edits this connection instead of creating one. */
  connection?: Connection;
}>();
const emit = defineEmits<{ saved: [] }>();

const toast = useToast();
const open = ref(false);

const editing = computed(() => Boolean(props.connection));

const providers = [
  { label: "GitHub", value: "github" },
  { label: "GitLab", value: "gitlab" },
  { label: "Gitea", value: "gitea" },
  { label: "Container registry", value: "dockerRegistry" },
  { label: "Neon", value: "neon" },
  { label: "CloudNativePG — Postgres the platform runs itself", value: "cnpg" },
  { label: "S3-compatible object store — MinIO, AWS S3, Cloudflare R2", value: "s3" },
  { label: "Inngest Cloud — durable background work", value: "inngest" },
  { label: "Inngest — durable background work, run by the platform itself", value: "inngestSelfHosted" },
  { label: "Valkey — a cache the platform runs itself", value: "valkey" },
  { label: "Redis — a cache or queue somewhere else: Upstash, ElastiCache, Aiven", value: "redis" },
];

const name = ref("");
const provider = ref("github");
const token = ref("");
const username = ref("");
const password = ref("");
// The config each provider understands: the registry prefix, a self-hosted
// GitHub's API URL, or where an object store is and how it is addressed.
const registryURL = ref("");
const apiURL = ref("");
const s3Endpoint = ref("");
const s3Region = ref("");
// Path style is what MinIO needs and AWS does not, and an application that
// guesses wrong fails on every request — so it is asked here, once, and
// travels in every binding. Scoped credentials is whether the platform mints
// a user per bucket through the MinIO admin API, which S3 and R2 lack.
const s3ForcePathStyle = ref(false);
const s3ScopedCredentials = ref(true);
const accessKeyId = ref("");
const secretAccessKey = ref("");
// A redis server's whole credential is its URL: the address and the password
// are the same string, and the scheme decides whether the connection is
// encrypted.
const redisURL = ref("");

// The three providers with nothing to store: CloudNativePG, the in-cluster
// Valkey and the self-hosted Inngest all provision with the operator's own
// account, so there is no credential, no field for one, and a credential
// sent anyway is refused rather than kept and never read.
const credentialLess = ["cnpg", "valkey", "inngestSelfHosted"];
const needsCredential = computed(() => !credentialLess.includes(provider.value));
const isS3 = computed(() => provider.value === "s3");
const isRedis = computed(() => provider.value === "redis");
const usesToken = computed(
  () => needsCredential.value && provider.value !== "dockerRegistry" && !isS3.value && !isRedis.value,
);

const guidance = computed(() => providerGuidance(provider.value, apiURL.value));

watch(open, (value) => {
  if (!value) return;
  // A fresh start on every open: prefill identity and config from the
  // connection being edited, never the credential.
  token.value = "";
  username.value = "";
  password.value = "";
  name.value = props.connection?.name ?? "";
  provider.value = props.connection?.provider ?? "github";
  registryURL.value = "";
  apiURL.value = "";
  s3Endpoint.value = "";
  s3Region.value = "";
  s3ForcePathStyle.value = false;
  s3ScopedCredentials.value = true;
  accessKeyId.value = "";
  secretAccessKey.value = "";
  redisURL.value = "";
  testResult.value = null;
  testError.value = "";
});

const credentialGiven = computed(() => {
  if (!needsCredential.value) return false;
  if (isS3.value) return Boolean(accessKeyId.value && secretAccessKey.value);
  if (isRedis.value) return Boolean(redisURL.value);
  return usesToken.value ? Boolean(token.value) : Boolean(username.value && password.value);
});

const ready = computed(() => {
  if (!editing.value && (!name.value || !provider.value)) return false;
  if (!editing.value && provider.value === "dockerRegistry" && !registryURL.value) return false;
  if (!editing.value && isS3.value && !s3Endpoint.value) return false;
  // Creating needs the credential; editing without one only changes config.
  if (!editing.value && needsCredential.value && !credentialGiven.value) return false;
  if (editing.value && !credentialGiven.value && !config.value) return false;
  return true;
});

const config = computed(() => {
  if (provider.value === "dockerRegistry" && registryURL.value) return { url: registryURL.value };
  if (provider.value === "github" && apiURL.value) return { apiUrl: apiURL.value };
  if (isS3.value && s3Endpoint.value) {
    return {
      endpoint: s3Endpoint.value.trim(),
      ...(s3Region.value.trim() ? { region: s3Region.value.trim() } : {}),
      forcePathStyle: s3ForcePathStyle.value,
      scopedCredentials: s3ScopedCredentials.value,
    };
  }
  return undefined;
});

const credential = computed(() => {
  if (isS3.value) return { accessKeyId: accessKeyId.value, secretAccessKey: secretAccessKey.value };
  if (isRedis.value) return { url: redisURL.value.trim() };
  return usesToken.value ? { token: token.value } : { username: username.value, password: password.value };
});

// The verdict of the last test, and the test's own failure — a 400 from the
// API is a badly formed request, not a provider's ruling, and reads as one.
const testResult = ref<ConnectionTestResult | null>(null);
const testError = ref("");
const testing = ref(false);

// Editing can re-check the stored credential without retyping it; creating
// has nothing to test until a credential is there.
// A provider with no credential is still testable, and the test is the useful
// one: whether the platform can run a database here at all.
const testable = computed(() => credentialGiven.value || editing.value || !needsCredential.value);

// Any change to what would be tested makes the old verdict stale.
watch(
  [provider, token, username, password, registryURL, apiURL, s3Endpoint, s3Region, s3ForcePathStyle, s3ScopedCredentials, accessKeyId, secretAccessKey, redisURL],
  () => {
    testResult.value = null;
    testError.value = "";
  },
);

async function test() {
  if (!testable.value || testing.value) return;
  testing.value = true;
  testResult.value = null;
  testError.value = "";
  try {
    testResult.value = await api.testConnection({
      // Creating deliberately sends no name: the probe is about the
      // credential, and the connection does not exist yet. Editing names it,
      // which is what lets a blank credential field mean "the stored one".
      name: editing.value ? props.connection!.name : undefined,
      provider: provider.value,
      config: config.value,
      credential: credentialGiven.value ? credential.value : undefined,
    });
  } catch (err) {
    testError.value = err instanceof Error ? err.message : String(err);
  } finally {
    testing.value = false;
  }
}

const saving = ref(false);
async function save() {
  if (!ready.value || saving.value) return;
  saving.value = true;
  try {
    if (editing.value) {
      await api.updateConnection(props.connection!.name, {
        config: config.value,
        credential: credentialGiven.value ? credential.value : undefined,
      });
      toast.add({
        title: credentialGiven.value
          ? `Credential for ${props.connection!.name} rotated`
          : `Connection ${props.connection!.name} updated`,
        color: "success",
        icon: "i-lucide-key-round",
      });
    } else {
      const created = await api.createConnection({
        name: name.value,
        provider: provider.value,
        config: config.value,
        // A provider with nothing to store sends nothing: the API refuses a
        // credential it would never read, because one somebody rotates
        // believing it matters is worse than none.
        credential: needsCredential.value ? credential.value : undefined,
      });
      toast.add({ title: `Connection ${created.name} created`, color: "success", icon: "i-lucide-plug" });
    }
    open.value = false;
    emit("saved");
  } catch (err) {
    toast.add({
      title: editing.value ? "Updating the connection failed" : "Creating the connection failed",
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
    :title="editing ? `Edit ${props.connection!.name}` : 'New connection'"
    :description="
      editing
        ? 'Rotate the credential or change the configuration. The stored credential is never shown; blank fields keep it.'
        : 'Link a git provider, registry, database or object store. The credential is stored by the operator and never read back.'
    "
  >
    <slot>
      <UButton icon="i-lucide-plus" size="sm">New connection</UButton>
    </slot>

    <template #body>
      <form class="space-y-4" @submit.prevent="save">
        <div v-if="!editing" class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Name" help="Lowercase letters, digits and dashes." required>
            <UInput v-model="name" placeholder="gh" class="w-full font-mono" autofocus />
          </UFormField>
          <UFormField label="Provider" required>
            <USelect v-model="provider" :items="providers" class="w-full" />
          </UFormField>
        </div>

        <UFormField
          v-if="provider === 'dockerRegistry'"
          label="Registry"
          help="The prefix images are pushed under — builds authenticate against its host."
          :required="!editing"
        >
          <UInput v-model="registryURL" placeholder="harbor.example.com/kitchen" class="w-full font-mono" />
        </UFormField>
        <UFormField
          v-if="provider === 'github'"
          label="API URL"
          help="Only for GitHub Enterprise — leave empty for github.com."
        >
          <UInput v-model="apiURL" placeholder="https://github.internal/api/v3" class="w-full font-mono" />
        </UFormField>

        <template v-if="isS3">
          <div class="grid gap-4 sm:grid-cols-2">
            <UFormField label="Endpoint" help="The store's URL, with its scheme." :required="!editing">
              <UInput v-model="s3Endpoint" placeholder="https://s3.eu-central-1.amazonaws.com" class="w-full font-mono" />
            </UFormField>
            <UFormField label="Region" help="Empty asks for us-east-1, which every store accepts.">
              <UInput v-model="s3Region" placeholder="eu-central-1" class="w-full font-mono" />
            </UFormField>
          </div>
          <UFormField
            label="Address buckets by path"
            help="On for MinIO and most self-hosted stores; off for AWS S3. It travels in every binding, so the application never has to guess."
          >
            <USwitch v-model="s3ForcePathStyle" />
          </UFormField>
          <UFormField
            label="Mint a credential per bucket"
            help="Through the MinIO admin API: a user and a policy scoped to each claim's bucket. Off for AWS S3, R2 and other stores without it — every claim is then handed this connection's own key pair."
          >
            <USwitch v-model="s3ScopedCredentials" />
          </UFormField>
        </template>

        <!-- A redis server's whole credential is its URL, so it is asked for
             as one field rather than a host and a password that would have to
             be recombined. rediss:// is the encrypted one, and the binding
             tells the application which it got. -->
        <UFormField
          v-if="isRedis"
          label="Server URL"
          help="redis:// or rediss:// — rediss is the encrypted one. The password is part of the URL."
          :required="!editing"
        >
          <UInput
            v-model="redisURL"
            type="password"
            placeholder="rediss://:password@eu2-x.upstash.io:6379"
            class="w-full font-mono"
          />
        </UFormField>

        <UFormField
          v-if="usesToken"
          :label="guidance?.tokenLabel ?? 'Token'"
          :help="editing ? 'Leave empty to keep the stored one.' : undefined"
          :required="!editing"
        >
          <UInput v-model="token" type="password" class="w-full font-mono" autocomplete="off" />
        </UFormField>

        <div v-if="isS3" class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Access key ID" :required="!editing">
            <UInput v-model="accessKeyId" class="w-full font-mono" autocomplete="off" />
          </UFormField>
          <UFormField
            label="Secret access key"
            :help="editing ? 'Leave both empty to keep the stored credential.' : undefined"
            :required="!editing"
          >
            <UInput v-model="secretAccessKey" type="password" class="w-full font-mono" autocomplete="off" />
          </UFormField>
        </div>

        <div v-if="!usesToken && !isS3" class="grid gap-4 sm:grid-cols-2">
          <UFormField label="Username" :required="!editing">
            <UInput v-model="username" class="w-full font-mono" autocomplete="off" />
          </UFormField>
          <UFormField
            label="Password"
            :help="editing ? 'Leave both empty to keep the stored credential.' : undefined"
            :required="!editing"
          >
            <UInput v-model="password" type="password" class="w-full font-mono" autocomplete="off" />
          </UFormField>
        </div>

        <!-- What the credential is for, what it has to be allowed to do, and
             the provider's own page for making one. -->
        <div v-if="guidance" class="rounded-md border border-default bg-elevated/50 px-3 py-2.5 space-y-1.5">
          <p class="text-xs text-toned">{{ guidance.purpose }}</p>
          <ul v-if="guidance.permissions.length" class="text-xs text-muted space-y-1">
            <li v-for="permission in guidance.permissions" :key="permission" class="flex gap-1.5">
              <span class="text-dimmed">•</span>
              <span>{{ permission }}</span>
            </li>
          </ul>
          <UButton
            v-if="guidance.link"
            :to="guidance.link.href"
            target="_blank"
            rel="noopener noreferrer"
            color="neutral"
            variant="link"
            size="xs"
            icon="i-lucide-external-link"
            class="px-0"
            >{{ guidance.link.label }}</UButton
          >
        </div>

        <!-- The probe's verdict, in the provider's own words. Reachability and
             the credential are separate answers, and the alert's colour says
             which one is the problem. -->
        <div v-if="testResult" class="space-y-2">
          <UAlert
            :color="
              testTone(testResult) === 'success' ? 'success' : testTone(testResult) === 'error' ? 'error' : 'warning'
            "
            variant="soft"
            :icon="testTone(testResult) === 'success' ? 'i-lucide-plug-zap' : 'i-lucide-triangle-alert'"
            :title="testSummary(testResult)"
            :description="testResult.message"
          />
          <!-- An accepted credential that is still short of something. The
               connection works, so this is a note beside the verdict rather
               than a colour on it. -->
          <ul v-if="testResult.warnings?.length" class="text-xs text-warning space-y-1 px-1">
            <li v-for="warning in testResult.warnings" :key="warning" class="flex gap-1.5">
              <UIcon name="i-lucide-triangle-alert" class="shrink-0 mt-0.5" />
              <span>{{ warning }}</span>
            </li>
          </ul>
        </div>
        <UAlert
          v-else-if="testError"
          color="error"
          variant="soft"
          icon="i-lucide-triangle-alert"
          title="The test could not run"
          :description="testError"
        />
      </form>
    </template>

    <template #footer>
      <div class="flex items-center gap-2 w-full">
        <!-- Testing writes nothing: it tries the credential against the
             provider and throws it away, so it is safe before creating. -->
        <UButton
          color="neutral"
          variant="subtle"
          icon="i-lucide-plug-zap"
          :disabled="!testable"
          :loading="testing"
          @click="test"
        >
          Test connection
        </UButton>
        <span class="flex-1" />
        <UButton color="neutral" variant="ghost" @click="open = false">Cancel</UButton>
        <UButton :disabled="!ready" :loading="saving" :icon="editing ? 'i-lucide-key-round' : 'i-lucide-plus'" @click="save">
          {{ editing ? "Save changes" : "Create connection" }}
        </UButton>
      </div>
    </template>
  </UModal>
</template>
