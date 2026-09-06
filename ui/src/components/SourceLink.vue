<script setup lang="ts">
/**
 * One identifier, linked back to the code it names (#435).
 *
 * The dashboard composes no URL. The API serves `commitUrl`, `branchUrl`,
 * `pullRequestUrl` and `repositoryUrl` on the objects that carry the facts,
 * because the host is a fact about the project's connection — a GitLab or
 * Gitea connection can name a forge anybody self-hosted — and three clients
 * deriving a URL scheme per provider is three chances to derive it
 * differently.
 *
 * So this takes the URL it is given and does one thing with it: renders a
 * link when there is one, and the same text unchanged when there is not. Every
 * one of these fields is absent for a build with no commit, a project whose
 * connection is gone, and a provider the platform has no web routing for, and
 * on all three the screen should read exactly as it did before links existed.
 *
 * A linked identifier takes the primary colour, which is what every other link
 * on these screens does; an unlinked one keeps whatever colour the row gave
 * it. So the colour is the difference between "this goes somewhere" and "this
 * is text", and a caller sets everything else — `font-mono`, a size — on the
 * component and it lands on whichever of the two renders.
 */
defineProps<{
  /** Where this identifier is on the provider's site. Absent renders text. */
  href?: string;
  /** The tooltip, on the link and on the plain text alike. */
  title?: string;
}>();
</script>

<template>
  <a v-if="href" :href="href" :title="title" target="_blank" rel="noopener" class="text-primary hover:underline">
    <slot />
  </a>
  <span v-else :title="title"><slot /></span>
</template>
