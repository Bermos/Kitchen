<script setup lang="ts">
/**
 * The mode gate, as one element.
 *
 * **Role decides what is permitted; mode decides what is rendered**
 * (docs/AUTH.md, "The UI split"). The role half is enforced twice over — the
 * API's own table, and the dashboard's generated copy of it — and the mode
 * half was, until now, enforced by whoever wrote the screen remembering to
 * write `v-if="operatorMode"`. Four screens remembered. The pod table on the
 * environment screen, the crash report's Kubernetes events and the log
 * screen's cluster switch did not, so an operator who chose the developer's
 * view kept being handed the operator's answers on the screens they use most.
 *
 * That is the bug this element exists to make hard to write again: operator
 * content is wrapped in something that says so, and `src/lib/design.test.ts`
 * refuses a developer screen that names a Pod, a Node, a namespace, a
 * manifest or a cluster Event outside one.
 *
 * It renders no wrapper of its own — the slot lands where the element was, so
 * it can sit inside a grid or a table body without changing the layout of
 * what surrounds it.
 */
import { operatorMode } from "../lib/mode";
</script>

<template>
  <slot v-if="operatorMode" />
</template>
