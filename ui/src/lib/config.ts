import { ref } from "vue";

// Runtime configuration for the SPA. The operator serves /config.json next to
// the static files, filled from the Kitchen singleton — so one build of the UI
// works on every installation. For `vite dev` (where the operator is only
// proxied for /api), the VITE_* variables fill the same fields.

export interface UIConfig {
  /** The OIDC issuer, e.g. https://auth.apps.example.com */
  issuer: string;
  /** OAuth client id the UI authenticates as. */
  clientId: string;
  /** The API's external URL — also the token audience (`resource`). */
  apiURL: string;
  /**
   * The release the operator serving this SPA was built from, as a bare
   * SemVer string — the UI adds the leading "v". One release publishes the
   * chart and both images, so this is the platform's version, not just the
   * dashboard's. "dev" for a build nothing stamped.
   */
  version: string;
}

let loaded: UIConfig | null = null;

/**
 * The version the operator serving this page reports, as a value screens can
 * render directly.
 *
 * It is a ref rather than a field read off `loadConfig()` because it is the one
 * part of the runtime config that can change while the page stays open: a
 * platform upgrade replaces the operator behind it. Everything that shows the
 * platform's version — the sidebar, the settings page — reads this, so they all
 * move together the moment the new operator answers.
 */
export const platformVersion = ref("");

export async function loadConfig(): Promise<UIConfig> {
  if (loaded) return loaded;
  let fetched: Partial<UIConfig> = {};
  try {
    const res = await fetch("/config.json", { headers: { accept: "application/json" } });
    if (res.ok) fetched = await res.json();
  } catch {
    // Dev server without an operator behind it — fall through to the env.
  }
  const env = import.meta.env;
  loaded = {
    issuer: fetched.issuer || env.VITE_ISSUER || "",
    clientId: fetched.clientId || env.VITE_CLIENT_ID || "kitchen-ui",
    apiURL: fetched.apiURL || env.VITE_API_URL || window.location.origin,
    version: fetched.version || env.VITE_VERSION || "dev",
  };
  platformVersion.value = loaded.version;
  return loaded;
}

/**
 * Ask /config.json for the running operator's version again, past the cache
 * `loadConfig` keeps.
 *
 * The cache is right for everything else on that document — the issuer, the
 * client id and the API's address are settled for the life of the page — and
 * wrong for exactly one field. During a platform upgrade the operator serving
 * this page is replaced, and the version the new one reports is the only signal
 * that it landed which does not need the authenticated API to be up: /config.json
 * is public, static and served by the same process. So this bypasses the cache
 * (and `cache: "no-store"`, so the browser does not answer from its own),
 * and writes what it finds back into both the cached config and
 * `platformVersion`.
 *
 * It throws while nothing is serving the file, which is what the caller polls
 * through: during the blackout every call fails, and the first one that
 * succeeds with a different number is the upgrade completing.
 */
export async function readVersion(): Promise<string> {
  const res = await fetch("/config.json", { headers: { accept: "application/json" }, cache: "no-store" });
  if (!res.ok) throw new Error(`/config.json answered ${res.status}`);
  const body = (await res.json()) as Partial<UIConfig>;
  const version = body.version;
  if (!version) throw new Error("/config.json carries no version");
  if (loaded && loaded.version !== version) loaded = { ...loaded, version };
  platformVersion.value = version;
  return version;
}
