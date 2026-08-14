import { ref, watch } from "vue";

// Developer mode shows what people deploy; operator mode also surfaces what
// the platform does underneath — `status.conditions` on every object, the
// coarse phase being only the summary (docs/CRDS.md).

const stored = localStorage.getItem("kitchen.mode");
export const operatorMode = ref(stored === "operator");

watch(operatorMode, (on) => {
  localStorage.setItem("kitchen.mode", on ? "operator" : "developer");
});
