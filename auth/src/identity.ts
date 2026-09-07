import type { Auth } from "./auth.js";
import type { Config } from "./config.js";

/**
 * Who the accounts in this identity provider are, and which of them are
 * people.
 *
 * There are four kinds of row in the user table and only one of them signs
 * in. The **service account** owns the operator's own credential. A **machine
 * account** owns one CI key and exists so that the key has a `sub` of its own
 * to be granted a project role with (docs/AUTH.md, "Machine accounts"). A
 * **platform credential's account** owns one key too, and exists so that a
 * scheduled job or an agent has a `sub` to be granted platform *scopes* with.
 * All three are credentials wearing an account's clothes, and the platform
 * must not count any of them as a person: an installation is bootstrapped when
 * a *person* exists, and the account directory the operator seeds its operator
 * list from and resolves addresses against is a directory of *people*.
 *
 * That line is drawn here and nowhere else. Two places drawing it slightly
 * differently is how the platform ends up believing it has an administrator
 * it does not have.
 */

/**
 * The domain every machine account's address sits under.
 *
 * A machine account needs an address because better-auth's user table needs
 * one, and it needs to be recognisable as a machine's because everything that
 * counts people has to skip it. A reserved domain does both without adding a
 * column: `.local` is reserved (RFC 6762) so no real mailbox can ever collide
 * with one, and an address here is deliberately never marked verified — an
 * access entry naming an address is honoured only for a verified one, so a
 * hand-written grant can never resolve to a machine account by address. The
 * only way to grant a key anything is its `sub`.
 *
 * It is a constant rather than configuration on purpose, and the operator has
 * the same constant: authorization is decided from the `sub` alone and never
 * from this address, but the operator does parse it to *display* a grant — a
 * project's members list shows a key as the key `nightly` rather than as a
 * person with a strange address (`MachineAccountDomain` in
 * internal/idp/keys.go). Changing the domain here without changing it there
 * breaks nothing and refuses nothing; it just quietly makes every CI key read
 * as an account nobody recognises. Change both, or neither.
 */
export const MACHINE_ACCOUNT_DOMAIN = "machines.kitchen.local";

/**
 * The domain every platform credential's account sits under.
 *
 * A platform credential (issue #349) is the third kind of row this file draws
 * a line around: an account owning one key, like a machine account, but
 * holding scopes on the *platform* instead of a role on one project. It gets a
 * reserved domain of its own rather than a reserved project under the one
 * above, for a reason that is structural rather than tidy: a machine account's
 * local part is `<project>.<key>`, and a project name is a single DNS label
 * with no dot in it, so a single-label local part under a second domain cannot
 * be shaped like a CI key's address and a CI key's cannot be shaped like this.
 * Reserving a project name instead would collide with whatever installation
 * already has a project by that name.
 *
 * Everything `isPerson` protects applies here identically, and for a sharper
 * reason: the platform seeds its operator list from the accounts that exist
 * when nobody has named any (docs/AUTH.md, "Bootstrap"), so an account counted
 * as a person here would be a credential that becomes an operator by upgrade.
 */
export const PLATFORM_ACCOUNT_DOMAIN = "platform.kitchen.local";

/**
 * The local part carries which project the key was made for and what it is
 * called: `shop.nightly@machines.kitchen.local` is the key `nightly` on the
 * project `shop`.
 *
 * That is the whole of "how a key is tied to a project". It is stored in the
 * account's identity rather than in the key's metadata, for two reasons.
 * Nothing then has to interpret key metadata to decide anything — the issue
 * this implements rules that out — and the tie is enforced by the user
 * table's unique address rather than by a convention somebody has to keep:
 * one key per name per project, and a name that is already taken collides at
 * the database rather than silently becoming a second credential nobody can
 * tell from the first.
 *
 * Both halves are DNS labels, which is why the separator can be a dot: a
 * label cannot contain one, so the split is unambiguous. Project names are
 * already DNS labels because they name Kubernetes objects; key names are held
 * to the same rule so that the address stays parseable and so that a key is
 * addressable by name in a URL path.
 */
const LABEL = /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/;

/** How the rule above reads when a refusal has to explain it. */
export const LABEL_RULE =
	"lowercase letters, digits and dashes, starting and ending with a letter or digit, at most 32 characters";

export function isLabel(value: string): boolean {
	return LABEL.test(value);
}

/**
 * Normalises an email address for comparison: addresses are not
 * case-sensitive, so an account created as Anna@Example.com is the one a
 * lookup for anna@example.com means.
 */
export function normalizeEmail(email: string): string {
	return email.trim().toLowerCase();
}

/** The address the machine account owning `key` on `project` is created as. */
export function machineAddress(project: string, key: string): string {
	return `${project}.${key}@${MACHINE_ACCOUNT_DOMAIN}`;
}

/** Whether an address belongs to a machine account rather than to a person. */
export function isMachineAccount(email: string): boolean {
	return normalizeEmail(email).endsWith(`@${MACHINE_ACCOUNT_DOMAIN}`);
}

/** The address the account owning the platform credential `name` is created as. */
export function platformAddress(name: string): string {
	return `${name}@${PLATFORM_ACCOUNT_DOMAIN}`;
}

/** Whether an address belongs to a platform credential's account. */
export function isPlatformAccount(email: string): boolean {
	return normalizeEmail(email).endsWith(`@${PLATFORM_ACCOUNT_DOMAIN}`);
}

/**
 * The credential name an address names, or null when it is not a platform
 * credential's — or is one this service did not write, which is the same
 * answer. The local part is one label, so an address carrying a dot in it is
 * not something anything here can act on.
 */
export function platformIdentity(email: string): string | null {
	if (!isPlatformAccount(email)) {
		return null;
	}
	const name = normalizeEmail(email).slice(0, -(PLATFORM_ACCOUNT_DOMAIN.length + 1));
	return isLabel(name) ? name : null;
}

/**
 * The project and key an address names, or null when it is not a machine
 * account's — or is one this service did not write, which is the same answer:
 * an address under the reserved domain whose local part is not two labels is
 * not something anything here can act on.
 */
export function machineIdentity(email: string): { project: string; key: string } | null {
	if (!isMachineAccount(email)) {
		return null;
	}
	const local = normalizeEmail(email).slice(0, -(MACHINE_ACCOUNT_DOMAIN.length + 1));
	const parts = local.split(".");
	if (parts.length !== 2) {
		return null;
	}
	const [project, key] = parts as [string, string];
	if (!isLabel(project) || !isLabel(key)) {
		return null;
	}
	return { project, key };
}

/**
 * Whether an address is the service account's — the one account the operator's
 * own credential belongs to, and the only one that may register an OAuth
 * client (`clientPrivileges` in src/auth.ts).
 */
export function isServiceAccount(config: Config, email: string | undefined | null): boolean {
	return Boolean(email) && normalizeEmail(email ?? "") === normalizeEmail(config.serviceAccountEmail);
}

/** Whether an address belongs to somebody who signs in. */
export function isPerson(config: Config, email: string): boolean {
	return !isServiceAccount(config, email) && !isMachineAccount(email) && !isPlatformAccount(email);
}

/** One row of the user table, reduced to what anything here reads. */
export interface PersonRow {
	id: string;
	email: string;
	name?: string | null;
	emailVerified?: boolean | null;
}

/**
 * Every account that belongs to a person, oldest first.
 *
 * The service account is excluded in the query, where an exact address can
 * be. Machine accounts are excluded afterwards, because the adapter's `Where`
 * has `ends_with` but no negation of it — and the account table is a team's
 * worth of rows, so one pass over it is cheaper than the alternative would be
 * to read.
 */
export async function listPeople(auth: Auth, config: Config): Promise<PersonRow[]> {
	const ctx = await auth.$context;
	const users = await ctx.adapter.findMany<PersonRow>({
		model: "user",
		where: [{ field: "email", operator: "ne", value: config.serviceAccountEmail }],
		sortBy: { field: "createdAt", direction: "asc" },
	});
	return users.filter((user) => isPerson(config, user.email));
}

/**
 * Where an account the platform creates came from.
 *
 * `internalAdapter.createUser` takes a provisioning source alongside the row
 * since better-auth 1.7: it is the input to the `user.validateUserInfo` gate,
 * which decides whether a provisioning attempt is allowed at all. Kitchen
 * configures no such gate — nothing here is provisioned from a provider's
 * profile — so today these values are recorded rather than read. They are
 * written honestly anyway, because the moment a gate is added it is handed
 * exactly this, and a gate that cannot tell a person's account from a
 * credential's is a gate that has to allow both.
 *
 * The split is the same one the rest of this file draws: `PERSON_PROVISIONING`
 * for the account somebody signs in to, which is created from an address and a
 * password; `MACHINE_PROVISIONING` for the service account and the machine
 * accounts, which are credentials wearing an account's clothes and are created
 * by the platform for itself. `method` is an open string for exactly this — no
 * built-in method describes an account nobody ever signs in to.
 */
export const PERSON_PROVISIONING = { method: "email-password" } as const;

/** @see PERSON_PROVISIONING */
export const MACHINE_PROVISIONING = { method: "kitchen-machine" } as const;
