<script setup lang="ts">
import { ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { isAuthenticated, signIn } from "../lib/auth";

const route = useRoute();
const router = useRouter();
const error = ref<string | null>(null);
const busy = ref(false);

if (isAuthenticated.value) {
  void router.replace((route.query.returnTo as string) || "/");
}

async function start() {
  busy.value = true;
  error.value = null;
  try {
    await signIn((route.query.returnTo as string) || "/");
  } catch (err) {
    error.value = err instanceof Error ? err.message : String(err);
    busy.value = false;
  }
}
</script>

<template>
  <div class="min-h-screen flex items-center justify-center px-4">
    <div class="w-full max-w-sm">
      <div class="flex items-center justify-center gap-2.5 mb-8">
        <img src="/favicon.svg" alt="" class="size-7" />
        <span class="text-xl font-semibold text-highlighted">Kitchen</span>
      </div>
      <UCard>
        <div class="space-y-4">
          <div>
            <h1 class="font-medium text-highlighted">Sign in</h1>
            <p class="text-sm text-muted mt-1">
              The dashboard signs in through the platform's identity provider. One login for Kitchen and every app it
              deploys.
            </p>
          </div>
          <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
          <UButton block :loading="busy" icon="i-lucide-log-in" @click="start">Continue to sign in</UButton>
        </div>
      </UCard>
      <p class="text-xs text-dimmed text-center mt-6 font-mono">cause we be cooking</p>
    </div>
  </div>
</template>
