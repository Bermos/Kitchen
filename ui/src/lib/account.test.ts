import { describe, expect, it } from "vitest";
import {
  deviceLabel,
  expiresIn,
  hasPassword,
  issuerMessage,
  sessionRows,
  upstreamProviders,
  type IssuerSession,
  type LinkedAccount,
} from "./account";

// The account screen renders what the identity provider answers, and the
// identity provider is better-auth: its vocabulary is not this platform's, and
// three of its refusals send people looking in the wrong place if they are
// passed through unread. These are about that translation, and about the
// sessions table being able to tell one browser from another.

const session = (over: Partial<IssuerSession> = {}): IssuerSession => ({
  id: "s1",
  token: "token-1",
  createdAt: "2026-08-01T10:00:00Z",
  updatedAt: "2026-08-01T10:00:00Z",
  expiresAt: "2026-08-08T10:00:00Z",
  ...over,
});

const account = (providerId: string): LinkedAccount => ({
  id: `a-${providerId}`,
  providerId,
  createdAt: "2026-08-01T10:00:00Z",
});

describe("what the identity provider refused, in this platform's words", () => {
  it("does not blame the dashboard's session for the issuer's", () => {
    // Both exist, they have different lifetimes, and only one of them has
    // just failed — the dashboard's is fine or this screen never loaded.
    const message = issuerMessage(401, "", "Unauthorized");

    expect(message).toContain("separate from the dashboard's");
    expect(message).toContain("sign-in cookie");
    expect(message).not.toBe("Unauthorized");
  });

  it("sends an untrusted origin to the operator, not to the person signed in", () => {
    const message = issuerMessage(403, "INVALID_ORIGIN", "Invalid origin");

    expect(message).toContain("operator");
    expect(message).toContain("auth.trustedOrigins");
  });

  it("says which password was wrong", () => {
    // "Invalid password" reads as a complaint about the new one.
    expect(issuerMessage(400, "INVALID_PASSWORD", "Invalid password")).toBe("that is not the current password");
  });

  it("passes anything else through as the issuer said it", () => {
    expect(issuerMessage(400, "PASSWORD_TOO_SHORT", "Password too short")).toBe("Password too short");
  });

  it("still says something when the refusal carried no words at all", () => {
    expect(issuerMessage(500, "", "")).toBe("the identity provider answered 500");
  });
});

describe("how this account signs in", () => {
  it("finds the password among the ways it can", () => {
    expect(hasPassword([account("credential")])).toBe(true);
    expect(hasPassword([account("github"), account("credential")])).toBe(true);
  });

  it("says there is none when the account came from upstream", () => {
    // Nothing to change, and the screen offers no form rather than a form the
    // issuer would refuse.
    expect(hasPassword([account("github")])).toBe(false);
    expect(hasPassword([])).toBe(false);
  });

  it("names the upstream providers and not the password", () => {
    expect(upstreamProviders([account("credential"), account("github")])).toEqual(["github"]);
  });
});

describe("telling one signed-in browser from another", () => {
  it("reads the browser and the system out of an ordinary agent", () => {
    expect(
      deviceLabel(
        "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0",
      ),
    ).toBe("Firefox on Linux");
    expect(
      deviceLabel(
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36",
      ),
    ).toBe("Chrome on macOS");
  });

  it("is not fooled by a browser claiming to be another", () => {
    // Every one of these carries "Safari", and Edge carries "Chrome" too.
    expect(
      deviceLabel(
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36 Edg/126.0",
      ),
    ).toBe("Edge on Windows");
    expect(
      deviceLabel(
        "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1",
      ),
    ).toBe("Safari on iOS");
  });

  it("shows an agent it does not know rather than calling it unknown", () => {
    // The string is the evidence: a session from something that is not a
    // browser is exactly what somebody is looking for here.
    expect(deviceLabel("kitchen-cli/0.16.1")).toBe("kitchen-cli/0.16.1");
    expect(deviceLabel(null)).toBe("an agent that sent no name");
  });
});

describe("the sessions table", () => {
  it("puts the newest first and marks this browser", () => {
    const rows = sessionRows(
      [
        session({ id: "old", token: "t-old", createdAt: "2026-08-01T10:00:00Z" }),
        session({ id: "new", token: "t-new", createdAt: "2026-08-20T10:00:00Z" }),
      ],
      "t-new",
    );

    expect(rows.map((row) => row.id)).toEqual(["new", "old"]);
    expect(rows[0].current).toBe(true);
    expect(rows[1].current).toBe(false);
  });

  it("marks nothing when the issuer named no current session", () => {
    // The safe direction: a row wrongly marked current is a session that
    // cannot be revoked from the only screen that can revoke it.
    const rows = sessionRows([session()], null);

    expect(rows[0].current).toBe(false);
  });

  it("leaves the answer it was given alone", () => {
    const answered = [session({ id: "a", createdAt: "2026-08-01T10:00:00Z" })];
    sessionRows(answered, null);

    expect(answered.map((row) => row.id)).toEqual(["a"]);
  });
});

describe("how long a session has left", () => {
  const now = Date.parse("2026-08-26T12:00:00Z");

  it("counts forwards, which is the whole reason it is not timeAgo", () => {
    // timeAgo measures backwards and answers "just now" for anything ahead of
    // it — on a row headed Expires that is the opposite of the truth.
    expect(expiresIn("2026-09-02T12:00:00Z", now)).toBe("in 7 days");
    expect(expiresIn("2026-08-27T09:00:00Z", now)).toBe("in 21 hours");
    expect(expiresIn("2026-08-26T12:30:00Z", now)).toBe("within the hour");
  });

  it("says so when it is already over", () => {
    expect(expiresIn("2026-08-25T12:00:00Z", now)).toBe("expired");
  });

  it("says nothing rather than NaN when there is no timestamp", () => {
    expect(expiresIn(undefined, now)).toBe("—");
    expect(expiresIn("not a date", now)).toBe("—");
  });
});
