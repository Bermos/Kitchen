<script setup lang="ts">
import { useRoute } from "vue-router";
import { computed } from "vue";
import AppShell from "./components/AppShell.vue";

const route = useRoute();
const bare = computed(() => route.meta.public === true);
</script>

<template>
  <UApp :toaster="{ position: 'bottom-right' }">
    <RouterView v-if="bare" />
    <AppShell v-else>
      <RouterView />
    </AppShell>
  </UApp>
</template>
