import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";

import { base32 } from "@better-auth/utils/base32";
import { createOTP } from "@better-auth/utils/otp";

import { returnTarget } from "../src/server.js";
import { startHarness, type Harness } from "./support.js";
import { SoftwareAuthenticator } from "./webauthn.js";

/**
 * A second factor, and a passkey instead of a password.
 *
 * Both plugins were mounted long before anything could reach them, which is
 * the state these tests exist to keep from coming back: every step here is one
 * the sign-in page or the dashboard's account screen takes, from the origin it
 * takes it from.
 */

const DASHBOARD = "https://kitchen.example.com";
const EMAIL = "anna@example.com";
const PASSWORD = "correct-horse-battery";

/**
 * A browser's cookies for one site: what a response set, replaced by name, and
 * dropped when the server expires them. The two-factor step lives entirely in
 * cookies — the pending sign-in, the WebAuthn challenge, the trusted device —
 * so a test that did not keep them would test nothing.
 */
class Jar {
	private readonly cookies = new Map<string, string>();

	take(response: Response): Response {
		for (const line of response.headers.getSetCookie()) {
			const [pair = "", ...attributes] = line.split(";");
			const at = pair.indexOf("=");
			const name = pair.slice(0, at).trim();
			const expired = attributes.some((attribute) => /^\s*max-age=0\s*$/i.test(attribute));
			if (expired || pair.slice(at + 1) === "") this.cookies.delete(name);
			else this.cookies.set(name, pair.slice(at + 1));
		}
		return response;
	}

	get header(): string {
		return [...this.cookies].map(([name, value]) => `${name}=${value}`).join("; ");
	}

	clear(): void {
		this.cookies.clear();
	}
}

describe("two-factor authentication and passkeys", () => {
	let kitchen: Harness;
	const jar = new Jar();

	/** A call from a page: the issuer's own unless another origin is named. */
	async function call(path: string, body?: unknown, origin?: string): Promise<Response> {
		const init: RequestInit = {
			headers: {
				origin: origin ?? kitchen.url,
				cookie: jar.header,
				...(body === undefined ? {} : { "content-type": "application/json" }),
			},
			...(body === undefined ? {} : { method: "POST", body: JSON.stringify(body) }),
		};
		return jar.take(await kitchen.fetch(path, init));
	}

	async function signIn(): Promise<Response> {
		return call("/sign-in/email", { email: EMAIL, password: PASSWORD });
	}

	async function signedInAs(): Promise<string | null> {
		const session = (await (await call("/get-session")).json()) as { user?: { email: string } } | null;
		return session?.user?.email ?? null;
	}

	before(async () => {
		kitchen = await startHarness({ apiURL: DASHBOARD });
		const created = await kitchen.fetch("/bootstrap", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ token: kitchen.bootstrapToken, email: EMAIL, name: "Anna", password: PASSWORD }),
		});
		assert.equal(created.status, 201, await created.clone().text());
	});

	after(async () => {
		await kitchen.stop();
	});

	describe("an authenticator app", () => {
		let secret = "";
		let backupCodes: string[] = [];

		before(async () => {
			jar.clear();
			assert.equal((await signIn()).status, 200);
		});

		it("is set up from the dashboard, proving the password, and is off until a code confirms it", async () => {
			const refused = await call("/two-factor/enable", { password: "not-it" }, DASHBOARD);
			assert.equal(refused.ok, false, "the password is what re-authenticates the change");

			const enabled = await call("/two-factor/enable", { password: PASSWORD }, DASHBOARD);
			assert.equal(enabled.status, 200, await enabled.clone().text());
			const answer = (await enabled.json()) as { totpURI: string; backupCodes: string[] };

			const uri = new URL(answer.totpURI);
			assert.equal(uri.protocol, "otpauth:");
			assert.match(
				decodeURIComponent(uri.pathname),
				/Kitchen \(127\.0\.0\.1\)/,
				"the installation is named, so two Kitchens in one app are two entries",
			);
			// The URI carries it base32-encoded, as every authenticator app
			// expects; the generator takes it as it was before encoding.
			const encoded = uri.searchParams.get("secret") ?? "";
			assert.ok(encoded, "the URI carries the secret the QR code encodes");
			secret = Buffer.from(base32.decode(encoded)).toString("utf8");
			backupCodes = answer.backupCodes;
			assert.ok(backupCodes.length >= 5, "backup codes arrive with the secret, once");

			const session = (await (await call("/get-session", undefined, DASHBOARD)).json()) as {
				user: { twoFactorEnabled?: boolean };
			};
			assert.equal(session.user.twoFactorEnabled, false, "a secret nobody has proved they hold is not a factor yet");
		});

		it("is switched on by the first code, and this browser stays signed in", async () => {
			const code = await createOTP(secret).totp();
			const verified = await call("/two-factor/verify-totp", { code }, DASHBOARD);
			assert.equal(verified.status, 200, await verified.clone().text());

			const session = (await (await call("/get-session", undefined, DASHBOARD)).json()) as {
				user: { twoFactorEnabled?: boolean };
			};
			assert.equal(session.user.twoFactorEnabled, true, "the dashboard reads this to show the state");
		});

		it("makes the password only the first step of signing in", async () => {
			jar.clear();
			const first = await signIn();
			assert.equal(first.status, 200);
			const body = (await first.json()) as { twoFactorRedirect?: boolean; twoFactorMethods?: string[] };
			assert.equal(body.twoFactorRedirect, true, "the page reads this to ask for the code");
			assert.deepEqual(body.twoFactorMethods, ["totp"]);
			assert.equal(await signedInAs(), null, "a password alone leaves no session");
		});

		it("refuses a wrong code and accepts the right one", async () => {
			const wrong = await call("/two-factor/verify-totp", { code: "000000" });
			assert.equal(wrong.ok, false);
			assert.equal(((await wrong.json()) as { code?: string }).code, "INVALID_CODE");
			assert.equal(await signedInAs(), null);

			const right = await call("/two-factor/verify-totp", { code: await createOTP(secret).totp() });
			assert.equal(right.status, 200, await right.clone().text());
			assert.equal(await signedInAs(), EMAIL);
		});

		it("takes a backup code instead, once", async () => {
			jar.clear();
			await signIn();
			const used = await call("/two-factor/verify-backup-code", { code: backupCodes[0] });
			assert.equal(used.status, 200, await used.clone().text());
			assert.equal(await signedInAs(), EMAIL);

			jar.clear();
			await signIn();
			const again = await call("/two-factor/verify-backup-code", { code: backupCodes[0] });
			assert.equal(again.ok, false, "a spent code is spent");
		});

		it("is turned off from the dashboard with the password, and signing in is one step again", async () => {
			jar.clear();
			await signIn();
			await call("/two-factor/verify-totp", { code: await createOTP(secret).totp() });

			const disabled = await call("/two-factor/disable", { password: PASSWORD }, DASHBOARD);
			assert.equal(disabled.status, 200, await disabled.clone().text());

			jar.clear();
			const body = (await (await signIn()).json()) as { twoFactorRedirect?: boolean };
			assert.equal(body.twoFactorRedirect, undefined);
			assert.equal(await signedInAs(), EMAIL);
		});
	});

	describe("a passkey", () => {
		const rpID = "127.0.0.1";
		const authenticator = new SoftwareAuthenticator(rpID);

		before(async () => {
			jar.clear();
			assert.equal((await signIn()).status, 200);
		});

		it("cannot be made from the dashboard's origin, which is why the issuer hosts the page", async () => {
			const stray = new SoftwareAuthenticator(rpID);
			const options = (await (await call("/passkey/generate-register-options", undefined, DASHBOARD)).json()) as {
				challenge: string;
			};
			const saved = await call(
				"/passkey/verify-registration",
				{ response: stray.create(options, { origin: DASHBOARD }) },
				DASHBOARD,
			);
			assert.equal(saved.ok, false, "a ceremony on another origin is refused");
		});

		it("is registered on the issuer's own page, bound to the issuer, and asks for user verification", async () => {
			const answer = await call("/passkey/generate-register-options");
			assert.equal(answer.status, 200, await answer.clone().text());
			const options = (await answer.json()) as {
				challenge: string;
				rp: { id: string };
				authenticatorSelection: { userVerification: string; residentKey: string };
			};
			assert.equal(options.rp.id, rpID);
			assert.equal(options.authenticatorSelection.userVerification, "required");
			assert.equal(options.authenticatorSelection.residentKey, "required", "it has to work with no address typed");

			const saved = await call("/passkey/verify-registration", {
				response: authenticator.create(options, { origin: kitchen.url }),
				name: "laptop",
			});
			assert.equal(saved.status, 200, await saved.clone().text());
		});

		it("is listed, renamed and kept from the dashboard", async () => {
			const listed = await call("/passkey/list-user-passkeys", undefined, DASHBOARD);
			assert.equal(listed.status, 200);
			const passkeys = (await listed.json()) as Array<{ id: string; name: string }>;
			assert.deepEqual(
				passkeys.map((passkey) => passkey.name),
				["laptop"],
			);

			const renamed = await call("/passkey/update-passkey", { id: passkeys[0]?.id, name: "work laptop" }, DASHBOARD);
			assert.equal(renamed.status, 200, await renamed.clone().text());
		});

		it("signs in with no password at all", async () => {
			jar.clear();
			const options = (await (await call("/passkey/generate-authenticate-options")).json()) as { challenge: string };
			const verified = await call("/passkey/verify-authentication", {
				response: authenticator.get(options, { origin: kitchen.url }),
			});
			assert.equal(verified.status, 200, await verified.clone().text());
			assert.equal(await signedInAs(), EMAIL);
		});

		it("is refused when the device did not verify its user, since it stands in for two factors", async () => {
			jar.clear();
			const options = (await (await call("/passkey/generate-authenticate-options")).json()) as { challenge: string };
			const refused = await call("/passkey/verify-authentication", {
				response: authenticator.get(options, { origin: kitchen.url, userVerified: false }),
			});
			assert.equal(refused.status, 401);
			assert.equal(((await refused.json()) as { code?: string }).code, "USER_VERIFICATION_REQUIRED");
			assert.equal(await signedInAs(), null);
		});

		it("is removed from the dashboard, and stops working", async () => {
			jar.clear();
			await signIn();
			const [passkey] = (await (await call("/passkey/list-user-passkeys", undefined, DASHBOARD)).json()) as Array<{
				id: string;
			}>;
			assert.ok(passkey);
			const removed = await call("/passkey/delete-passkey", { id: passkey.id }, DASHBOARD);
			assert.equal(removed.status, 200, await removed.clone().text());

			jar.clear();
			const options = (await (await call("/passkey/generate-authenticate-options")).json()) as { challenge: string };
			const refused = await call("/passkey/verify-authentication", {
				response: authenticator.get(options, { origin: kitchen.url }),
			});
			assert.equal(refused.ok, false);
		});
	});

	describe("the pages", () => {
		it("offer a passkey and the second step on the sign-in page", async () => {
			const html = await (await kitchen.fetch("/login")).text();
			assert.match(html, /Sign in with a passkey/);
			assert.match(html, /autocomplete="username webauthn"/, "so the browser can offer a passkey in autofill");
			assert.match(html, /\/two-factor\/verify-totp/);
			assert.match(html, /\/two-factor\/verify-backup-code/);
		});

		it("serve the passkey page, and send the browser back only to the dashboard", async () => {
			const back = encodeURIComponent(`${DASHBOARD}/account`);
			const html = await (await kitchen.fetch(`/passkeys/new?return_to=${back}`)).text();
			assert.match(html, /\/passkey\/generate-register-options/);
			assert.match(html, /data-return="https:\/\/kitchen\.example\.com\/account"/);

			const elsewhere = await (
				await kitchen.fetch(`/passkeys/new?return_to=${encodeURIComponent("https://evil.example.net/")}`)
			).text();
			assert.match(elsewhere, /data-return=""/);
		});

		it("honour a return address on a trusted origin and nothing else", () => {
			const allowed = new Set([DASHBOARD]);
			assert.equal(returnTarget(`${DASHBOARD}/account`, allowed), `${DASHBOARD}/account`);
			assert.equal(returnTarget("https://evil.example.net/account", allowed), null);
			assert.equal(returnTarget("javascript:alert(1)", allowed), null);
			assert.equal(returnTarget("/account", allowed), null, "a bare path names no origin to check");
			assert.equal(returnTarget(null, allowed), null);
		});
	});
});
