<script setup lang="ts">
import { onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { completeSignIn } from "../lib/auth";

const router = useRouter();
const error = ref<string | null>(null);

onMounted(async () => {
  try {
    const returnTo = await completeSignIn(new URLSearchParams(window.location.search));
    await router.replace(returnTo);
  } catch (err) {
    error.value = err instanceof Error ? err.message : String(err);
  }
});
</script>

<template>
  <div class="min-h-screen flex items-center justify-center px-4">
    <div v-if="error" class="w-full max-w-md space-y-4">
      <UAlert color="error" variant="soft" icon="i-lucide-triangle-alert" title="Sign-in failed" :description="error" />
      <UButton to="/login" color="neutral" variant="subtle" icon="i-lucide-rotate-ccw">Try again</UButton>
    </div>
    <p v-else class="text-muted text-sm">Signing in…</p>
  </div>
</template>
