/**
 * The three pages the identity provider has to serve itself: sign-in, consent
 * and first-run bootstrap. They are deliberately plain — the Kitchen UI is a
 * separate application and will take over the hosted pages when it lands, at
 * which point these become the fallback for installs without a UI.
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

export function loginPage(options: { github: boolean }): string {
	return page(
		"Sign in",
		`
<h1>Sign in</h1>
<p class="sub">Kitchen accounts are also used to sign in to the apps deployed here.</p>
<form id="form">
  <label>Email<input type="email" name="email" autocomplete="username" required></label>
  <label>Password<input type="password" name="password" autocomplete="current-password" required></label>
  <p class="error" id="error"></p>
  <button type="submit">Sign in</button>
</form>
${options.github ? `<div class="row" style="margin-top:.75rem"><button class="secondary" id="github">Continue with GitHub</button></div>` : ""}
`,
		`
const params = location.search;
const next = () => params.includes("client_id=") ? "/oauth2/authorize" + params : "/";
const error = document.getElementById("error");
document.getElementById("form").addEventListener("submit", async (event) => {
  event.preventDefault();
  error.textContent = "";
  const data = new FormData(event.target);
  const response = await fetch("/sign-in/email", {
    method: "POST",
    headers: { "content-type": "application/json" },
    credentials: "include",
    body: JSON.stringify({ email: data.get("email"), password: data.get("password") }),
  });
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    error.textContent = body.message || "Sign-in failed.";
    return;
  }
  location.href = next();
});
const github = document.getElementById("github");
if (github) github.addEventListener("click", async () => {
  const response = await fetch("/sign-in/social", {
    method: "POST",
    headers: { "content-type": "application/json" },
    credentials: "include",
    body: JSON.stringify({ provider: "github", callbackURL: next() }),
  });
  const body = await response.json().catch(() => ({}));
  if (body.url) location.href = body.url;
  else error.textContent = body.message || "GitHub sign-in is unavailable.";
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
