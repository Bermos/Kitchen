import { computed, ref, type WritableComputedRef } from "vue";
import { isOperator, platformRole } from "./me";
import { platformAtLeast } from "./policy";

/**
 * Which of the two dashboards is being rendered.
 *
 * Developer mode shows what people deploy; operator mode also surfaces what
 * the platform does underneath — `status.conditions` on every object, the
 * coarse phase being only the summary (docs/CRDS.md), and the materialized
 * Kubernetes objects behind an environment.
 *
 * **Role decides what is permitted, mode decides what is rendered, and mode
 * defaults from role** (docs/AUTH.md, "The UI split"). The toggle was a free
 * switch before any of this was enforced; what it is now is an operator's own
 * choice to look at the platform the way a developer does. A member has no
 * such choice, because there is nothing on the other side of it for them: the
 * conditions tables they would get are on objects the API answers with, but
 * the objects fetch behind operator mode is operator-only, so flipping it used
 * to buy them a panel of 403s.
 *
 * That is why `operatorMode` is derived rather than stored. What is stored is
 * a *preference*, and a preference is only ever consulted for an account that
 * may act on it.
 */

/** The two modes. */
export type Mode = "developer" | "operator";

/** Where the preference lives. Named here because a stale value under this
 * key is the case this module exists to get right. */
export const MODE_STORAGE_KEY = "kitchen.mode";

/**
 * The mode a stored preference and a platform role add up to.
 *
 * Three rules, and the third is the one that needed writing down:
 *
 * - A member is in developer mode, whatever is stored. **Every installation
 *   upgrading into enforcement has members with `kitchen.mode=operator` in
 *   localStorage**, because until now the switch was free and flipping it was
 *   the only way to see a conditions table. Reading that value back would put
 *   them straight into the dashboard this change exists to take away from
 *   them. It is ignored rather than deleted: it says what they picked, and it
 *   means something again if they are ever made an operator.
 * - An operator with no preference lands in operator mode. It is their
 *   dashboard; arriving in the other one and having to find the switch is the
 *   wrong first screen.
 * - An operator who has picked developer mode gets developer mode. That was
 *   the point of the toggle and it survives.
 */
export function modeFor(stored: string | null, role: string | undefined): Mode {
  if (!platformAtLeast(role, "operator")) return "developer";
  return stored === "developer" ? "developer" : "operator";
}

/** Whether this account may switch at all — an operator may, a member has
 * nothing to switch to. The toggle is hidden rather than disabled where the
 * answer is no, on the same reasoning every other affordance here follows. */
export const canSwitchMode = isOperator;

function readPreference(): string | null {
  try {
    return localStorage.getItem(MODE_STORAGE_KEY);
  } catch {
    // No storage — a private-mode browser, or a test. The role alone decides.
    return null;
  }
}

function writePreference(mode: Mode): void {
  try {
    localStorage.setItem(MODE_STORAGE_KEY, mode);
  } catch {
    // The preference is a convenience; not being able to keep it is not an
    // error worth surfacing.
  }
}

// The stored preference, held as a ref so that flipping the switch
// re-renders without reading storage on every access.
const preference = ref<string | null>(readPreference());

/** The mode in effect: the preference, narrowed by what the role allows. */
export const mode = computed<Mode>(() => modeFor(preference.value, platformRole.value));

/**
 * The mode as the boolean every screen asks it as, writable so the header's
 * toggle can set it.
 *
 * A write from an account that may not switch is dropped rather than stored:
 * the control does not exist for them, and a stored value they cannot act on
 * is the stale preference this module already has to defend against.
 */
export const operatorMode: WritableComputedRef<boolean> = computed({
  get: () => mode.value === "operator",
  set: (on: boolean) => {
    if (!canSwitchMode.value) return;
    const next: Mode = on ? "operator" : "developer";
    preference.value = next;
    writePreference(next);
  },
});
