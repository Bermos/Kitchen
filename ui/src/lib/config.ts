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
}

let loaded: UIConfig | null = null;

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
  };
  return loaded;
}
