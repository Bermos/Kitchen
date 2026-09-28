import { createHash, generateKeyPairSync, randomBytes, sign, type KeyObject } from "node:crypto";

/**
 * A software authenticator: just enough of WebAuthn to register a passkey and
 * sign in with it, so the passkey plugin is exercised end to end rather than
 * trusted.
 *
 * It makes an ES256 key, packs the public half into a `none` attestation the
 * way a platform authenticator does, and answers an assertion with a signature
 * over exactly what a browser would hand the server. The flags are the part a
 * test turns: `userVerified` is the PIN or biometric, and the one Kitchen
 * insists on.
 */

/** The binary fields of WebAuthn's JSON shapes are base64url, unpadded. */
export const b64u = (bytes: Uint8Array): string => Buffer.from(bytes).toString("base64url");

const sha256 = (data: Uint8Array | string): Buffer => createHash("sha256").update(data).digest();

type CBOR = number | string | Uint8Array | Map<CBOR, CBOR>;

function head(major: number, length: number): Buffer {
	if (length < 24) return Buffer.from([(major << 5) | length]);
	if (length < 0x100) return Buffer.from([(major << 5) | 24, length]);
	if (length < 0x10000) return Buffer.from([(major << 5) | 25, length >> 8, length & 0xff]);
	const out = Buffer.alloc(5);
	out[0] = (major << 5) | 26;
	out.writeUInt32BE(length, 1);
	return out;
}

/** The corner of CBOR an attestation needs: integers, strings, bytes, maps. */
function cbor(value: CBOR): Buffer {
	if (typeof value === "number") return value >= 0 ? head(0, value) : head(1, -1 - value);
	if (typeof value === "string") {
		const bytes = Buffer.from(value, "utf8");
		return Buffer.concat([head(3, bytes.length), bytes]);
	}
	if (value instanceof Map) {
		const parts = [head(5, value.size)];
		for (const [key, item] of value) parts.push(cbor(key), cbor(item));
		return Buffer.concat(parts);
	}
	return Buffer.concat([head(2, value.length), Buffer.from(value)]);
}

const UP = 0x01;
const UV = 0x04;
const AT = 0x40;

function counter(value: number): Buffer {
	const out = Buffer.alloc(4);
	out.writeUInt32BE(value);
	return out;
}

export interface Ceremony {
	userVerified?: boolean;
	/** The page the ceremony ran on — what the browser writes into the client data. */
	origin: string;
}

export class SoftwareAuthenticator {
	private readonly privateKey: KeyObject;
	private readonly publicKey: KeyObject;
	readonly credentialID = randomBytes(16);
	private signCount = 0;

	constructor(private readonly rpID: string) {
		const pair = generateKeyPairSync("ec", { namedCurve: "P-256" });
		this.privateKey = pair.privateKey;
		this.publicKey = pair.publicKey;
	}

	private flags(extra: number, userVerified: boolean | undefined): number {
		return UP | extra | (userVerified === false ? 0 : UV);
	}

	/** `navigator.credentials.create`, answered: RegistrationResponseJSON. */
	create(options: { challenge: string }, ceremony: Ceremony): Record<string, unknown> {
		const jwk = this.publicKey.export({ format: "jwk" });
		const coseKey = cbor(
			new Map<CBOR, CBOR>([
				[1, 2], // kty: EC2
				[3, -7], // alg: ES256
				[-1, 1], // crv: P-256
				[-2, Buffer.from(jwk.x ?? "", "base64url")],
				[-3, Buffer.from(jwk.y ?? "", "base64url")],
			]),
		);
		const idLength = Buffer.alloc(2);
		idLength.writeUInt16BE(this.credentialID.length);
		const authData = Buffer.concat([
			sha256(this.rpID),
			Buffer.from([this.flags(AT, ceremony.userVerified)]),
			counter(this.signCount),
			Buffer.alloc(16), // the all-zero AAGUID of a `none` attestation
			idLength,
			this.credentialID,
			coseKey,
		]);
		const attestationObject = cbor(
			new Map<CBOR, CBOR>([
				["fmt", "none"],
				["attStmt", new Map()],
				["authData", authData],
			]),
		);
		const clientDataJSON = Buffer.from(
			JSON.stringify({ type: "webauthn.create", challenge: options.challenge, origin: ceremony.origin, crossOrigin: false }),
		);
		return {
			id: b64u(this.credentialID),
			rawId: b64u(this.credentialID),
			type: "public-key",
			authenticatorAttachment: "platform",
			clientExtensionResults: {},
			response: {
				clientDataJSON: b64u(clientDataJSON),
				attestationObject: b64u(attestationObject),
				transports: ["internal"],
			},
		};
	}

	/** `navigator.credentials.get`, answered: AuthenticationResponseJSON. */
	get(options: { challenge: string }, ceremony: Ceremony): Record<string, unknown> {
		this.signCount += 1;
		const authenticatorData = Buffer.concat([
			sha256(this.rpID),
			Buffer.from([this.flags(0, ceremony.userVerified)]),
			counter(this.signCount),
		]);
		const clientDataJSON = Buffer.from(
			JSON.stringify({ type: "webauthn.get", challenge: options.challenge, origin: ceremony.origin, crossOrigin: false }),
		);
		const signature = sign("sha256", Buffer.concat([authenticatorData, sha256(clientDataJSON)]), this.privateKey);
		return {
			id: b64u(this.credentialID),
			rawId: b64u(this.credentialID),
			type: "public-key",
			authenticatorAttachment: "platform",
			clientExtensionResults: {},
			response: {
				clientDataJSON: b64u(clientDataJSON),
				authenticatorData: b64u(authenticatorData),
				signature: b64u(signature),
			},
		};
	}
}
