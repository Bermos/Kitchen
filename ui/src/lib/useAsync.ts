import { onScopeDispose, ref, shallowRef, type Ref, type ShallowRef } from "vue";
import { APIError } from "./api";
import { router } from "../router";

export interface AsyncState<T> {
  data: ShallowRef<T | null>;
  error: Ref<string | null>;
  loading: Ref<boolean>;
  refresh: () => Promise<void>;
}

/** Fetch-and-hold for a view: one load on setup, `refresh()` for polling and
 * after writes. A 401 sends the browser back through the login. */
export function useAsync<T>(fetcher: () => Promise<T>, { immediate = true } = {}): AsyncState<T> {
  const data = shallowRef<T | null>(null);
  const error = ref<string | null>(null);
  const loading = ref(false);
  let alive = true;
  onScopeDispose(() => {
    alive = false;
  });

  const refresh = async () => {
    loading.value = true;
    try {
      const result = await fetcher();
      if (!alive) return;
      data.value = result;
      error.value = null;
    } catch (err) {
      if (!alive) return;
      if (err instanceof APIError && err.status === 401) {
        void router.push({ name: "login", query: { returnTo: router.currentRoute.value.fullPath } });
        return;
      }
      error.value = err instanceof Error ? err.message : String(err);
    } finally {
      if (alive) loading.value = false;
    }
  };

  if (immediate) void refresh();
  return { data, error, loading, refresh };
}

/** Poll while `active()` says so — running builds, deploying environments. */
export function usePoll(callback: () => void, intervalMs: number, active: () => boolean): void {
  const timer = setInterval(() => {
    if (active()) callback();
  }, intervalMs);
  onScopeDispose(() => clearInterval(timer));
}
