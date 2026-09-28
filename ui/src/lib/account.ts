import { renderSVG } from "uqr";
import { loadConfig } from "./config";

/**
 * An account, as the person who owns it manages it.
 *
 * This is the one part of the dashboard that talks to the **identity
 * provider** rather than to the operator API, and that is the design rather
 * than an inconsistency. A password is a credential, and Kitchen's rule is
 * that the API never reads one back and never handles one — routing a password
 * change through the operator would put every password on this platform
 * through a service that has no business seeing one, to reach an endpoint the
 * issuer already mounts. So the browser talks to the issuer directly, the same
 * way it does for discovery, the token exchange and revocation
 * (`auth.ts`), and the operator stays out of it.
 *
 * **The credential here is the issuer's own session cookie**, not the bearer
 * token the rest of the dashboard uses. That cookie was set by the sign-in
 * page at `<issuer>/login`, and every call below is a cross-origin request
 * carrying it — which needs three things to hold, all of them arranged
 * elsewhere:
 *
 * - the issuer reflects this origin in its CORS headers, so the browser lets
 *   the answer be read (`allowedOrigins`, auth/src/config.ts);
 * - the issuer *trusts* this origin, so better-auth does not refuse the write
 *   as cross-site (`trustedOrigins`, the same list);
 * - the browser is willing to send the cookie at all, which holds while the
 *   dashboard and the issuer are subdomains of one site — the chart's default,
 *   and not something the dashboard can arrange for an installation that moved
 *   `auth.host` elsewhere. `issuerMessage` is what says so when it does not.
 *
 * Nothing here is cached: an account screen is opened to change something, and
 * the answer to "what do I look like right now" has to be this second's.
 */

/** One of the identity provider's sessions: a browser that is signed in. */
export interface IssuerSession {
  id: string;
  /** What `revoke-session` takes. Also how the current one is recognised. */
  token: string;
  createdAt: string;
  updatedAt: string;
  expiresAt: string;
  ipAddress?: string | null;
  userAgent?: string | null;
}

/** A way of signing in that this account has: a password, or an upstream
 * provider it arrived through. */
export interface LinkedAccount {
  id: string;
  /** `credential` for a password; otherwise the provider's id (`github`). */
  providerId: string;
  createdAt: string;
}

/** The account as the issuer holds it. The dashboard's own idea of who is
 * signed in comes off the access token instead (`auth.ts`), which is a
 * snapshot taken at sign-in — so this is the one that is current. */
export interface IssuerAccount {
  id: string;
  name: string;
  email: string;
  emailVerified: boolean;
  /** Whether signing in with the password asks for a code too. Absent from an
   * issuer that never mounted two-factor, which reads the same as off. */
  twoFactorEnabled?: boolean | null;
}

/** A passkey this account can sign in with, as the issuer lists it. Nothing
 * here is a secret: the private half never left the device that made it. */
export interface Passkey {
  id: string;
  name?: string | null;
  /** `singleDevice` for a hardware key; `multiDevice` for one a password
   * manager or a phone's keychain syncs between devices. */
  deviceType: string;
  backedUp: boolean;
  createdAt: string;
  aaguid?: string | null;
}

/** What turning the authenticator app on hands back, once. */
export interface TOTPEnrollment {
  /** `otpauth://totp/…` — what the QR code carries. */
  totpURI: string;
  backupCodes: string[];
}

/** What the identity provider refused, and why, in a sentence somebody can act
 * on. `code` is better-auth's, kept so a caller can tell one refusal from
 * another without reading the prose. */
export class IssuerError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "IssuerError";
  }
}

/** The provider id better-auth gives a password. Anything else is upstream. */
export const PASSWORD_PROVIDER = "credential";

/**
 * better-auth's own minimum, spelled out here so the form can say it before
 * the round trip rather than after. It is the default the service leaves in
 * place (`auth/src/auth.ts` sets neither bound), and the bootstrap form checks
 * the same two numbers.
 */
export const MIN_PASSWORD_LENGTH = 8;
export const MAX_PASSWORD_LENGTH = 128;

/**
 * What went wrong, said in terms of this platform rather than of better-auth.
 *
 * A few of these are worth translating because the raw answer sends people
 * looking in the wrong place:
 *
 * - **401** is not "your dashboard session expired" — the dashboard's session
 *   is fine, or none of this screen would have loaded. It is the *issuer's*
 *   session, which is a different thing with a different lifetime, and which
 *   the browser may also simply have declined to send.
 * - **403 `INVALID_ORIGIN`** is an installation that has not told its identity
 *   provider where its dashboard is. Nobody signed in can fix that, so the
 *   sentence names the operator and the value.
 * - **`INVALID_PASSWORD`** is the current password being wrong, which reads as
 *   a bizarre thing to be told when the new one is what is being typed.
 *
 * Everything else is better-auth's own message, which is generally a sentence
 * already. A refusal with no message at all still says the status, because
 * "something went wrong" is not a thing to report to an operator.
 */
export function issuerMessage(status: number, code: string, message: string): string {
  // Ahead of the 401 below, because the issuer answers a wrong code with one:
  // read as a session that expired, it would send somebody off to sign in
  // again over a typo.
  if (code === "INVALID_CODE") {
    return "that code is not the one the authenticator app is showing now — check the device's clock if it keeps happening";
  }
  if (status === 401) {
    return (
      "the identity provider does not recognise this browser — its sign-in is separate from the dashboard's " +
      "and may have expired, so signing out and in again is the first thing to try. If that changes nothing, " +
      "this dashboard and the identity provider are not on the same site and the browser will not send the " +
      "sign-in cookie between them (docs/AUTH.md, \"Managing an account\")"
    );
  }
  if (code === "INVALID_ORIGIN") {
    return (
      "the identity provider does not trust this dashboard's address, so it refuses to act on a request from " +
      "it — an operator adds the address to the platform's API URL or to auth.trustedOrigins"
    );
  }
  if (code === "INVALID_PASSWORD") {
    return "that is not the current password";
  }
  if (code === "TOTP_ALREADY_ENABLED") {
    return "an authenticator app is already set up for this account — turn two-factor off first to replace it";
  }
  if (code === "CREDENTIAL_ACCOUNT_NOT_FOUND") {
    return "this account has no password to change: it signs in through an upstream provider";
  }
  return message || `the identity provider answered ${status}`;
}

/** Whether this account signs in with a password at all. An account created
 * by an upstream provider has none, and there is nothing here to change. */
export function hasPassword(accounts: LinkedAccount[]): boolean {
  return accounts.some((account) => account.providerId === PASSWORD_PROVIDER);
}

/** The upstream providers this account signs in through, in the order the
 * issuer answered. Empty for a password-only account. */
export function upstreamProviders(accounts: LinkedAccount[]): string[] {
  return accounts.filter((account) => account.providerId !== PASSWORD_PROVIDER).map((account) => account.providerId);
}

/**
 * A user agent as a phrase, for telling one row of the sessions table from
 * another.
 *
 * This is deliberately crude. The question the table answers is "is one of
 * these not me?", and for that "Firefox on Linux, from 10.0.0.4" is the whole
 * of what a browser string is worth — parsing it properly would mean shipping
 * a user-agent database to render six words. An agent nothing here recognises
 * is shown as it arrived, truncated, rather than as "unknown": the string
 * itself is the evidence, and a session with no agent at all is worth seeing
 * as exactly that.
 */
export function deviceLabel(userAgent: string | null | undefined): string {
  const agent = (userAgent ?? "").trim();
  if (!agent) return "an agent that sent no name";

  // Order matters: every one of these puts "Safari" or "Chrome" in its string
  // to be taken for one, so the most specific claim has to be read first.
  const browsers: Array<[RegExp, string]> = [
    [/\bEdg[A-Za-z]*\//, "Edge"],
    [/\bOPR\/|\bOpera\//, "Opera"],
    [/\bFirefox\/|\bFxiOS\//, "Firefox"],
    [/\bChrome\/|\bCriOS\//, "Chrome"],
    [/\bSafari\//, "Safari"],
  ];
  const systems: Array<[RegExp, string]> = [
    [/\bAndroid\b/, "Android"],
    [/\biPhone\b|\biPad\b|\biOS\b/, "iOS"],
    [/\bWindows\b/, "Windows"],
    [/\bMac OS X\b|\bMacintosh\b/, "macOS"],
    [/\bCrOS\b/, "ChromeOS"],
    [/\bLinux\b/, "Linux"],
  ];

  const browser = browsers.find(([pattern]) => pattern.test(agent))?.[1];
  const system = systems.find(([pattern]) => pattern.test(agent))?.[1];
  if (browser && system) return `${browser} on ${system}`;
  if (browser) return browser;
  if (system) return system;
  return agent.length > 40 ? `${agent.slice(0, 39)}…` : agent;
}

/**
 * How long a session has left, as a phrase.
 *
 * `timeAgo` cannot answer this: it measures backwards, and a timestamp in the
 * future comes back from it as "just now" — which on a row headed *Expires* is
 * not merely imprecise but the opposite of the truth. Hours are worth showing
 * on the last day; below that the number stops being interesting, because
 * nothing here is decided in minutes.
 */
export function expiresIn(iso: string | undefined, now: number = Date.now()): string {
  if (!iso) return "—";
  const at = new Date(iso).getTime();
  if (Number.isNaN(at)) return "—";
  const seconds = Math.round((at - now) / 1000);
  if (seconds <= 0) return "expired";
  const days = Math.floor(seconds / 86_400);
  if (days >= 1) return `in ${days} day${days === 1 ? "" : "s"}`;
  const hours = Math.floor(seconds / 3_600);
  if (hours >= 1) return `in ${hours} hour${hours === 1 ? "" : "s"}`;
  return "within the hour";
}

/** One row of the sessions table. */
export interface SessionRow extends IssuerSession {
  /** The browser this screen is being read in. It is offered no revoke
   * button: ending it from here would sign the reader out mid-sentence, and
   * the account menu already has a control that does that on purpose. */
  current: boolean;
  device: string;
}

/**
 * The sessions table: newest first, this browser marked.
 *
 * "This browser" is the session whose token the issuer just named as the
 * current one. When it names none — an older issuer, or an answer that did not
 * arrive — nothing is marked rather than something being guessed, and every
 * row keeps its revoke button. That is the safe direction: a row wrongly
 * marked current is a session that cannot be revoked from the only screen that
 * can revoke it.
 */
export function sessionRows(sessions: IssuerSession[], currentToken: string | null): SessionRow[] {
  return [...sessions]
    .sort((a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime())
    .map((session) => ({
      ...session,
      current: Boolean(currentToken) && session.token === currentToken,
      device: deviceLabel(session.userAgent),
    }));
}

/** The issuer's origin, without the trailing slash a configured URL may carry. */
async function issuerURL(): Promise<string> {
  const config = await loadConfig();
  if (!config.issuer) {
    throw new IssuerError(0, "NO_ISSUER", "no identity provider is configured for this dashboard");
  }
  return config.issuer.replace(/\/$/, "");
}

/**
 * One call to the issuer, carrying its session cookie.
 *
 * `credentials: "include"` is the whole point of the function: without it the
 * browser sends a cross-origin request with no cookie, and every one of these
 * endpoints answers 401 — which looks exactly like a session that expired.
 */
async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const issuer = await issuerURL();
  let response: Response;
  try {
    response = await fetch(`${issuer}${path}`, {
      ...init,
      credentials: "include",
      headers: { accept: "application/json", ...init.headers },
    });
  } catch {
    // A network-level failure here is usually the CORS preflight being
    // refused, which the browser reports to the script as an opaque failure.
    throw new IssuerError(0, "UNREACHABLE", `the identity provider at ${issuer} could not be reached`);
  }

  let body: { code?: string; message?: string } | null = null;
  if (response.status !== 204) {
    try {
      body = (await response.json()) as { code?: string; message?: string };
    } catch {
      body = null;
    }
  }
  if (!response.ok) {
    const code = body?.code ?? "";
    throw new IssuerError(response.status, code, issuerMessage(response.status, code, body?.message ?? ""));
  }
  return body as T;
}

function post<T>(path: string, body: unknown): Promise<T> {
  return request<T>(path, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
}

/** Who the issuer thinks this browser is, and which of its sessions this is. */
export async function currentSession(): Promise<{ account: IssuerAccount; token: string | null }> {
  const answer = await request<{ user: IssuerAccount; session: { token?: string } } | null>("/get-session");
  if (!answer?.user) {
    throw new IssuerError(401, "UNAUTHORIZED", issuerMessage(401, "", ""));
  }
  return { account: answer.user, token: answer.session?.token ?? null };
}

/** The ways this account can sign in. */
export function listAccounts(): Promise<LinkedAccount[]> {
  return request<LinkedAccount[]>("/list-accounts");
}

/** Every browser currently signed in as this account. */
export function listSessions(): Promise<IssuerSession[]> {
  return request<IssuerSession[]>("/list-sessions");
}

/** Change the display name. The address is not changeable here and the issuer
 * refuses it outright — see the screen, and docs/AUTH.md. */
export function changeName(name: string): Promise<{ status: boolean }> {
  return post("/update-user", { name });
}

/**
 * Change the password, proving the old one.
 *
 * `revokeOtherSessions` is offered rather than assumed: a password changed
 * because it was weak is not the same event as one changed because somebody
 * else may know it, and only the person typing knows which this is. When it is
 * set the issuer mints a new session for this browser and sets the cookie, so
 * the screen the change was made on stays signed in.
 */
export function changePassword(
  currentPassword: string,
  newPassword: string,
  revokeOtherSessions: boolean,
): Promise<unknown> {
  return post("/change-password", { currentPassword, newPassword, revokeOtherSessions });
}

/** End one of this account's sessions at the issuer. */
export function revokeSession(token: string): Promise<{ status: boolean }> {
  return post("/revoke-session", { token });
}

// --- a second factor --------------------------------------------------------
//
// An authenticator app, asked for after the password on the issuer's sign-in
// page. It guards the password and only the password: an account that signs
// in through GitHub or an SSO provider was vouched for there, and a passkey is
// two factors by itself (the device, and the PIN or biometric that unlocks it
// — the issuer refuses one that skipped the second). Every write below proves
// the password, because the question it answers is "is it really you turning
// this off", which a session cookie cannot answer.

/**
 * Start setting up an authenticator app. The secret is stored but is not a
 * factor yet: `confirmTOTP` with the first code is what switches it on, so a
 * QR code nobody scanned cannot lock anybody out.
 */
export function enableTOTP(password: string): Promise<TOTPEnrollment> {
  return post("/two-factor/enable", { password });
}

/** Finish setting it up, proving the app holds the secret. The issuer answers
 * by replacing this browser's session with one that knows two-factor is on. */
export function confirmTOTP(code: string): Promise<unknown> {
  return post("/two-factor/verify-totp", { code: code.replace(/\s+/g, "") });
}

/** Turn it off. Backup codes and any browser trusted to skip it go with it. */
export function disableTwoFactor(password: string): Promise<unknown> {
  return post("/two-factor/disable", { password });
}

/** A fresh set of backup codes, replacing every earlier one. */
export function regenerateBackupCodes(password: string): Promise<{ backupCodes: string[] }> {
  return post("/two-factor/generate-backup-codes", { password });
}

/**
 * The secret out of an `otpauth://` URI, grouped in fours for typing into an
 * app by hand — what somebody does when the camera is on the device showing
 * the QR code. Empty for anything that is not such a URI.
 */
export function totpSecret(uri: string): string {
  try {
    const secret = new URL(uri).searchParams.get("secret") ?? "";
    return secret.replace(/(.{4})(?=.)/g, "$1 ");
  } catch {
    return "";
  }
}

/**
 * The QR code for an `otpauth://` URI, as an image address.
 *
 * Drawn here rather than fetched: the secret is in the URI, so sending it to
 * a QR service would publish the second factor, and the dashboard's policy
 * lets images come from itself and `data:` alone.
 */
export function totpQRCode(uri: string): string {
  const svg = renderSVG(uri, { ecc: "M", border: 2, pixelSize: 6 });
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`;
}

// --- passkeys ---------------------------------------------------------------
//
// Listed, renamed and removed from here; *made* on a page the issuer serves.
// A passkey belongs to a relying party — the issuer's hostname — and the
// browser refuses to run the ceremony that creates one from a page on any
// other, so the dashboard links there and the issuer links back.

export function listPasskeys(): Promise<Passkey[]> {
  return request<Passkey[]>("/passkey/list-user-passkeys");
}

export function renamePasskey(id: string, name: string): Promise<unknown> {
  return post("/passkey/update-passkey", { id, name });
}

export function deletePasskey(id: string): Promise<unknown> {
  return post("/passkey/delete-passkey", { id });
}

/** Where to send the browser to make a passkey, and how it gets back here.
 * The issuer honours the return address only on an origin it trusts. */
export async function passkeyEnrollmentURL(returnTo: string): Promise<string> {
  const issuer = await issuerURL();
  return `${issuer}/passkeys/new?return_to=${encodeURIComponent(returnTo)}`;
}

/** What a passkey row is called: its name, or failing that what kind of thing
 * holds it — which is the only other thing the issuer knows about it. */
export function passkeyLabel(passkey: Passkey): string {
  const name = passkey.name?.trim();
  if (name) return name;
  return passkey.deviceType === "multiDevice" ? "A synced passkey" : "A security key";
}
