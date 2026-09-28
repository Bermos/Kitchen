/**
 * The pages the identity provider has to serve itself: sign-in, consent,
 * first-run bootstrap and registering a passkey. They are deliberately plain —
 * the Kitchen UI is a separate application and will take over the hosted pages
 * when it lands, at which point these become the fallback for installs without
 * a UI.
 *
 * Registering a passkey is here rather than on the dashboard's account screen
 * for a reason that no UI can take over: a passkey belongs to the relying
 * party, which is this service's hostname, and WebAuthn refuses a ceremony run
 * from a page on any other. The dashboard lists and removes passkeys; it links
 * here to make one.
 */

const STYLE = `
:root { color-scheme: light dark; --fg: #16181d; --bg: #fbfbfd; --muted: #5b6472;
  --line: #d8dce4; --accent: #1f6feb; --accent-fg: #ffffff; --danger: #b3261e; }
@media (prefers-color-scheme: dark) {
  :root { --fg: #e7e9ee; --bg: #14161a; --muted: #98a1b0; --line: #2b303a;
    --accent: #4c8dff; --accent-fg: #0b0d10; --danger: #ff9b93; }
}
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 2rem 1rem;
  background: var(--bg); color: var(--fg);
  font: 15px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
main { width: 100%; max-width: 26rem; }
h1 { font-size: 1.25rem; margin: 0 0 .25rem; }
p.sub { margin: 0 0 1.5rem; color: var(--muted); }
form { display: grid; gap: .75rem; }
label { display: grid; gap: .25rem; font-size: .85rem; color: var(--muted); }
input { padding: .55rem .7rem; border: 1px solid var(--line); border-radius: .4rem;
  background: transparent; color: inherit; font: inherit; }
button { padding: .55rem .7rem; border-radius: .4rem; border: 1px solid transparent;
  background: var(--accent); color: var(--accent-fg); font: inherit; font-weight: 600; cursor: pointer; }
button.secondary { background: transparent; border-color: var(--line); color: inherit; font-weight: 400; }
.row { display: flex; gap: .5rem; }
.row > * { flex: 1; }
.scopes { margin: 0 0 1.25rem; padding-left: 1.1rem; color: var(--muted); }
.error { color: var(--danger); min-height: 1.25rem; font-size: .85rem; }
.hidden { display: none; }
.check { display: flex; gap: .5rem; align-items: center; font-size: .85rem; color: var(--muted); }
.check input { margin: 0; }
.divider { display: flex; align-items: center; gap: .75rem; margin: 1rem 0; color: var(--muted); font-size: .8rem; }
.divider::before, .divider::after { content: ""; flex: 1; border-top: 1px solid var(--line); }
a { color: var(--accent); }
button.link { background: none; border: 0; padding: 0; color: var(--accent); font-weight: 400; text-align: left; }
.brand { font-size: .8rem; letter-spacing: .08em; text-transform: uppercase; color: var(--muted); margin-bottom: 1.5rem; }
code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
`;

function page(title: string, body: string, script = ""): string {
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>${escapeHTML(title)} · Kitchen</title>
<style>${STYLE}</style>
</head>
<body>
<main>
<p class="brand">Kitchen</p>
${body}
</main>
${script ? `<script>${script}</script>` : ""}
</body>
</html>
`;
}

export function escapeHTML(value: string): string {
	return value
		.replaceAll("&", "&amp;")
		.replaceAll("<", "&lt;")
		.replaceAll(">", "&gt;")
		.replaceAll('"', "&quot;")
		.replaceAll("'", "&#39;");
}

/**
 * WebAuthn in the browser, without a bundler.
 *
 * The server side speaks the JSON shapes of SimpleWebAuthn — every binary field
 * base64url — and `navigator.credentials` speaks ArrayBuffers. Browsers that
 * have `PublicKeyCredential.parse*OptionsFromJSON` and `credential.toJSON()`
 * convert for themselves; the rest are converted here, which is a dozen lines
 * rather than a dependency the pages would need a build step to carry.
 */
const WEBAUTHN = `
const b64u = {
  decode: (s) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4)), (c) => c.charCodeAt(0)),
  encode: (buffer) => btoa(String.fromCharCode(...new Uint8Array(buffer))).replace(/\\+/g, "-").replace(/\\//g, "_").replace(/=+$/, ""),
};
const withIds = (list) => (list || []).map((item) => ({ ...item, id: b64u.decode(item.id) }));
function requestOptions(json) {
  if (PublicKeyCredential.parseRequestOptionsFromJSON) return PublicKeyCredential.parseRequestOptionsFromJSON(json);
  return { ...json, challenge: b64u.decode(json.challenge), allowCredentials: withIds(json.allowCredentials) };
}
function creationOptions(json) {
  if (PublicKeyCredential.parseCreationOptionsFromJSON) return PublicKeyCredential.parseCreationOptionsFromJSON(json);
  return { ...json, challenge: b64u.decode(json.challenge), user: { ...json.user, id: b64u.decode(json.user.id) },
    excludeCredentials: withIds(json.excludeCredentials) };
}
function credentialJSON(credential) {
  if (typeof credential.toJSON === "function") return credential.toJSON();
  const r = credential.response;
  const response = { clientDataJSON: b64u.encode(r.clientDataJSON) };
  if (r.attestationObject) {
    response.attestationObject = b64u.encode(r.attestationObject);
    response.transports = r.getTransports ? r.getTransports() : [];
  } else {
    response.authenticatorData = b64u.encode(r.authenticatorData);
    response.signature = b64u.encode(r.signature);
    if (r.userHandle) response.userHandle = b64u.encode(r.userHandle);
  }
  return { id: credential.id, rawId: b64u.encode(credential.rawId), type: credential.type, response,
    authenticatorAttachment: credential.authenticatorAttachment ?? undefined,
    clientExtensionResults: credential.getClientExtensionResults() };
}
const passkeysSupported = () => typeof window.PublicKeyCredential === "function";
async function call(path, body) {
  const response = await fetch(path, body === undefined
    ? { credentials: "include" }
    : { method: "POST", headers: { "content-type": "application/json" }, credentials: "include", body: JSON.stringify(body) });
  const answer = await response.json().catch(() => ({}));
  return { ok: response.ok, body: answer || {} };
}
`;

/**
 * Where to go once signed in, as the page's own script computes it.
 *
 * An authorization request in flight is resumed; otherwise a `next` naming a
 * path on this origin is followed — which is how the passkey page sends
 * somebody here and gets them back — and anything else, a full URL above all,
 * is ignored, so the parameter cannot be turned into a redirect off-site.
 */
const NEXT = `
const params = location.search;
function next() {
  if (params.includes("client_id=")) return "/oauth2/authorize" + params;
  const wanted = new URLSearchParams(params).get("next") || "";
  if (wanted.startsWith("/") && !wanted.startsWith("//") && !wanted.startsWith("/\\\\")) return wanted;
  return "/";
}
`;

export function loginPage(options: { github: boolean }): string {
	return page(
		"Sign in",
		`
<section id="first">
<h1>Sign in</h1>
<p class="sub">Kitchen accounts are also used to sign in to the apps deployed here.</p>
<form id="form">
  <label>Email<input type="email" name="email" autocomplete="username webauthn" required></label>
  <label>Password<input type="password" name="password" autocomplete="current-password" required></label>
  <p class="error" id="error"></p>
  <button type="submit">Sign in</button>
</form>
<div class="row" style="margin-top:.75rem"><button class="secondary" id="passkey" type="button">Sign in with a passkey</button></div>
${options.github ? `<div class="row" style="margin-top:.75rem"><button class="secondary" id="github" type="button">Continue with GitHub</button></div>` : ""}
</section>
<section id="second" class="hidden">
<h1>Two-factor authentication</h1>
<p class="sub" id="second-sub">Enter the six-digit code from your authenticator app.</p>
<form id="code-form">
  <label><span id="code-label">Code</span><input name="code" id="code" autocomplete="one-time-code" inputmode="numeric" required></label>
  <label class="check"><input type="checkbox" name="trust" id="trust"> Don't ask again on this browser for 30 days</label>
  <p class="error" id="code-error"></p>
  <button type="submit">Verify</button>
</form>
<p style="margin-top:1rem"><button class="link" id="use-backup" type="button">Use a backup code instead</button></p>
</section>
`,
		`${WEBAUTHN}${NEXT}
const error = document.getElementById("error");

// --- password, then (if the account has one) the second factor -------------
let backup = false;
function askForCode() {
  document.getElementById("first").classList.add("hidden");
  document.getElementById("second").classList.remove("hidden");
  document.getElementById("code").focus();
}
document.getElementById("use-backup").addEventListener("click", () => {
  backup = !backup;
  const code = document.getElementById("code");
  code.value = "";
  code.setAttribute("inputmode", backup ? "text" : "numeric");
  code.setAttribute("autocomplete", backup ? "off" : "one-time-code");
  document.getElementById("code-label").textContent = backup ? "Backup code" : "Code";
  document.getElementById("second-sub").textContent = backup
    ? "Enter one of the backup codes you saved when you turned two-factor on. Each works once."
    : "Enter the six-digit code from your authenticator app.";
  document.getElementById("use-backup").textContent = backup ? "Use the authenticator app instead" : "Use a backup code instead";
  code.focus();
});
document.getElementById("form").addEventListener("submit", async (event) => {
  event.preventDefault();
  error.textContent = "";
  const data = new FormData(event.target);
  const { ok, body } = await call("/sign-in/email", { email: data.get("email"), password: data.get("password") });
  if (!ok) {
    error.textContent = body.message || "Sign-in failed.";
    return;
  }
  if (body.twoFactorRedirect) {
    askForCode();
    return;
  }
  location.href = next();
});
document.getElementById("code-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const codeError = document.getElementById("code-error");
  codeError.textContent = "";
  const code = document.getElementById("code").value.trim();
  const trustDevice = document.getElementById("trust").checked;
  const { ok, body } = await call(backup ? "/two-factor/verify-backup-code" : "/two-factor/verify-totp", { code, trustDevice });
  if (ok) {
    location.href = next();
    return;
  }
  // The pending sign-in lasts ten minutes and five wrong codes; past either
  // there is nothing left to put a code against, and saying "wrong code"
  // again would send somebody round a loop that cannot succeed.
  if (body.code === "INVALID_TWO_FACTOR_COOKIE" || body.code === "TOO_MANY_ATTEMPTS_REQUEST_NEW_CODE") {
    codeError.textContent = "This sign-in has expired. Sign in with your password again.";
    setTimeout(() => location.reload(), 2500);
    return;
  }
  codeError.textContent = body.message || "That code was not accepted.";
});

// --- passkeys ---------------------------------------------------------------
// A passkey is offered two ways: in the browser's own autofill on the email
// field, where the browser supports that, and behind the button, which works
// everywhere passkeys do. Only one ceremony can be open at a time, so the
// button cancels the autofill one before starting its own.
let conditional = null;
async function signInWithPasskey(mediation) {
  const { ok, body: options } = await call("/passkey/generate-authenticate-options");
  if (!ok) throw new Error(options.message || "Passkey sign-in is unavailable.");
  const request = { publicKey: requestOptions(options) };
  if (mediation) {
    conditional = new AbortController();
    request.mediation = mediation;
    request.signal = conditional.signal;
  }
  const credential = await navigator.credentials.get(request);
  const verified = await call("/passkey/verify-authentication", { response: credentialJSON(credential) });
  if (!verified.ok) throw new Error(verified.body.message || "That passkey was not accepted.");
  location.href = next();
}
const passkeyButton = document.getElementById("passkey");
if (!passkeysSupported()) {
  passkeyButton.parentElement.classList.add("hidden");
} else {
  passkeyButton.addEventListener("click", async () => {
    error.textContent = "";
    if (conditional) conditional.abort();
    try {
      await signInWithPasskey();
    } catch (err) {
      // Cancelling the browser's dialog is a choice rather than a failure.
      if (err && err.name === "NotAllowedError") return;
      error.textContent = err && err.message ? err.message : "Passkey sign-in failed.";
    }
  });
  if (PublicKeyCredential.isConditionalMediationAvailable) {
    PublicKeyCredential.isConditionalMediationAvailable().then((available) => {
      if (!available) return;
      signInWithPasskey("conditional").catch((err) => {
        if (err && (err.name === "AbortError" || err.name === "NotAllowedError")) return;
        error.textContent = err && err.message ? err.message : "Passkey sign-in failed.";
      });
    });
  }
}

const github = document.getElementById("github");
if (github) github.addEventListener("click", async () => {
  const { body } = await call("/sign-in/social", { provider: "github", callbackURL: next() });
  if (body.url) location.href = body.url;
  else error.textContent = body.message || "GitHub sign-in is unavailable.";
});
`,
	);
}

/**
 * Registering a passkey for the account this browser is signed in as.
 *
 * `returnTo` is where to send the browser afterwards — the dashboard's account
 * screen, which links here — and it has already been checked against the
 * origins this service trusts (src/server.ts); anything else arrives as `null`
 * and the page simply stays put when it is done.
 */
export function passkeyPage(options: { returnTo: string | null }): string {
	const back = options.returnTo
		? `<p style="margin-top:1rem"><a href="${escapeHTML(options.returnTo)}">Back to the dashboard</a></p>`
		: "";
	return page(
		"Add a passkey",
		`
<h1>Add a passkey</h1>
<p class="sub" id="who">Checking who is signed in…</p>
<section id="signed-out" class="hidden">
  <p class="sub">Sign in first, and you will be brought back here.</p>
  <div class="row"><button id="sign-in" type="button">Sign in</button></div>
</section>
<form id="form" class="hidden" data-return="${escapeHTML(options.returnTo ?? "")}">
  <label>Name<input name="name" id="name" maxlength="64" placeholder="Laptop, phone, security key" autocomplete="off"></label>
  <p class="error" id="error"></p>
  <button type="submit">Create passkey</button>
</form>
<p class="sub hidden" id="done"></p>
${back}
`,
		`${WEBAUTHN}
const form = document.getElementById("form");
const error = document.getElementById("error");
const who = document.getElementById("who");
document.getElementById("sign-in").addEventListener("click", () => {
  location.href = "/login?next=" + encodeURIComponent(location.pathname + location.search);
});
(async () => {
  if (!passkeysSupported()) {
    who.textContent = "This browser cannot create passkeys.";
    return;
  }
  const { body } = await call("/get-session");
  if (!body || !body.user) {
    who.textContent = "Nobody is signed in on this browser.";
    document.getElementById("signed-out").classList.remove("hidden");
    return;
  }
  who.textContent = "For " + body.user.email + ". Your device will ask you to confirm with its PIN, fingerprint or face — that is what lets a passkey sign in on its own, without a password or a code.";
  form.classList.remove("hidden");
  document.getElementById("name").focus();
})();
form.addEventListener("submit", async (event) => {
  event.preventDefault();
  error.textContent = "";
  try {
    const { ok, body: options } = await call("/passkey/generate-register-options");
    if (!ok) throw new Error(options.message || "Could not start creating a passkey.");
    const credential = await navigator.credentials.create({ publicKey: creationOptions(options) });
    const name = document.getElementById("name").value.trim();
    const saved = await call("/passkey/verify-registration", { response: credentialJSON(credential), ...(name ? { name } : {}) });
    if (!saved.ok) throw new Error(saved.body.message || "The passkey was not saved.");
  } catch (err) {
    if (err && err.name === "NotAllowedError") {
      error.textContent = "Cancelled — nothing was created.";
      return;
    }
    if (err && err.name === "InvalidStateError") {
      error.textContent = "This device already holds a passkey for this account.";
      return;
    }
    error.textContent = err && err.message ? err.message : "Could not create the passkey.";
    return;
  }
  form.classList.add("hidden");
  who.textContent = "Passkey saved. Next time, choose “Sign in with a passkey” on the sign-in page.";
  const back = form.dataset.return;
  if (back) {
    document.getElementById("done").textContent = "Taking you back…";
    document.getElementById("done").classList.remove("hidden");
    setTimeout(() => { location.href = back; }, 1200);
  }
});
`,
	);
}

export function consentPage(options: { clientName: string; scopes: string[] }): string {
	const scopes = options.scopes.length > 0 ? options.scopes : ["openid"];
	return page(
		"Authorize",
		`
<h1>Authorize ${escapeHTML(options.clientName)}</h1>
<p class="sub">This application is asking for access to your Kitchen account.</p>
<ul class="scopes">${scopes.map((scope) => `<li><code>${escapeHTML(scope)}</code></li>`).join("")}</ul>
<p class="error" id="error"></p>
<div class="row">
  <button class="secondary" id="deny">Deny</button>
  <button id="allow">Allow</button>
</div>
`,
		`
const error = document.getElementById("error");
async function decide(accept) {
  const response = await fetch("/oauth2/consent", {
    method: "POST",
    headers: { "content-type": "application/json" },
    credentials: "include",
    body: JSON.stringify({ accept, oauth_query: location.search.slice(1) }),
  });
  // A browser fetch is answered with the redirect as JSON rather than a 302.
  const body = await response.json().catch(() => ({}));
  const target = body.url || body.redirect_uri;
  if (target) location.href = target;
  else error.textContent = body.message || "Could not complete authorization.";
}
document.getElementById("allow").addEventListener("click", () => decide(true));
document.getElementById("deny").addEventListener("click", () => decide(false));
`,
	);
}

export function bootstrapPage(token: string): string {
	return page(
		"First administrator",
		`
<h1>Create the first administrator</h1>
<p class="sub">This link works once: it stops working as soon as this installation has an account.</p>
<form id="form">
  <input type="hidden" name="token" value="${escapeHTML(token)}">
  <label>Name<input name="name" autocomplete="name" required></label>
  <label>Email<input type="email" name="email" autocomplete="username" required></label>
  <label>Password<input type="password" name="password" autocomplete="new-password" required minlength="8"></label>
  <p class="error" id="error"></p>
  <button type="submit">Create account</button>
</form>
`,
		`
const error = document.getElementById("error");
document.getElementById("form").addEventListener("submit", async (event) => {
  event.preventDefault();
  error.textContent = "";
  const data = Object.fromEntries(new FormData(event.target));
  const response = await fetch("/bootstrap", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(data),
  });
  const body = await response.json().catch(() => ({}));
  if (response.ok) location.href = "/login";
  else error.textContent = body.error || "Could not create the account.";
});
`,
	);
}

export function messagePage(title: string, message: string): string {
	return page(title, `<h1>${escapeHTML(title)}</h1><p class="sub">${escapeHTML(message)}</p>`);
}
