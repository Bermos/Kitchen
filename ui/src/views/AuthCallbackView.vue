<script setup lang="ts">
import { onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { completeSignIn } from "../lib/auth";
import { isOperator, loadMe } from "../lib/me";

const router = useRouter();
const error = ref<string | null>(null);

// Where an operator lands, and why it is decided here rather than remembered.
//
// The Platform scope is the operator's estate and the first screen they want;
// the fleet dashboard is everybody else's. The landing is *fixed* rather than
// sticky on purpose — a remembered last scope is exactly the stale preference
// the four scopes replaced (#469, decision 5), and it would put somebody in
// the operator's estate because of where they happened to be a week ago.
//
// It only applies where the sign-in had no destination of its own: a returnTo
// is a link somebody followed, and it wins.
onMounted(async () => {
  try {
    const returnTo = await completeSignIn(new URLSearchParams(window.location.search));
    await loadMe();
    await router.replace(returnTo === "/" && isOperator.value ? "/platform" : returnTo);
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
