import { describe, expect, it } from "vitest";
import type { Alert } from "./api";
import {
  MAX_SILENCE_DAYS,
  alertsSentence,
  audienceLabel,
  defaultSilenceUntil,
  mitigationSentence,
  silenced,
  sortAlerts,
  tierLabel,
  tierRank,
  tierTone,
} from "./alerts";

const alert = (over: Partial<Alert> = {}): Alert => ({
  signal: "workload.crashloop",
  severity: "critical",
  scope: { kind: "environment", project: "shop", environment: "shop-production", name: "app" },
  fingerprint: "workload.crashloop/shop/shop-production/app",
  title: "crash-looping",
  detail: "12 restarts in 30m",
  since: "2026-08-16T09:31:00Z",
  evidence: "/environments/shop-production",
  audience: "developer",
  tier: "page",
  actionable: true,
  ...over,
});

describe("the word the top tier does not get", () => {
  // #471, decision 5: there is a delivery mechanism and nothing in it wakes a
  // person, so the dashboard does not say "page" until something behind it
  // actually pages. A screen saying it would be the platform claiming a
  // capability it does not have, and the first anybody would hear otherwise is
  // the morning after an outage.
  it("is not used anywhere a person reads", () => {
    for (const tier of ["page", "ticket", "log"] as const) {
      expect(tierLabel(tier).toLowerCase()).not.toContain("page");
    }
  });

  it("still orders the three the way the model does", () => {
    expect(tierRank("page")).toBeGreaterThan(tierRank("ticket"));
    expect(tierRank("ticket")).toBeGreaterThan(tierRank("log"));
  });

  it("names each for what it asks of the reader", () => {
    expect(tierLabel("page")).toBe("Act now");
    expect(tierLabel("ticket")).toBe("Needs a fix");
    expect(tierLabel("log")).toBe("For the record");
  });

  // The row's tone is the tier's and deliberately not the severity's: a
  // critical condition somebody is already fixing is a ticket, and reading as
  // a five-alarm row would be the list lying about what is left to do.
  it("colours by the tier rather than by the severity", () => {
    expect(tierTone("page")).toBe("error");
    expect(tierTone("ticket")).toBe("warning");
    expect(tierTone("log")).toBe("neutral");
  });
});

describe("sorting", () => {
  it("puts what needs acting on now first", () => {
    const rows = sortAlerts([
      alert({ tier: "log", fingerprint: "a" }),
      alert({ tier: "page", fingerprint: "b" }),
      alert({ tier: "ticket", fingerprint: "c" }),
    ]);
    expect(rows.map((row) => row.tier)).toEqual(["page", "ticket", "log"]);
  });

  // One condition, two deliveries. They are two rows and must not collapse
  // into one, which is the whole of why the audience is part of the order.
  it("keeps a condition's two deliveries apart", () => {
    const rows = sortAlerts([
      alert({ audience: "operator", tier: "ticket" }),
      alert({ audience: "developer", tier: "ticket" }),
    ]);
    expect(rows).toHaveLength(2);
    expect(rows.map((row) => row.audience)).toEqual(["developer", "operator"]);
  });
});

describe("what is being done about it", () => {
  it("says nothing when nothing is", () => {
    expect(mitigationSentence(alert())).toBe("");
    expect(mitigationSentence(alert({ mitigation: {} }))).toBe("");
  });

  // "Rolled back" reads as something that happened; "acknowledged" reads as a
  // button somebody pressed. The API says which, so the screen says it too.
  it("tells an implicit acknowledgement from a pressed button", () => {
    expect(
      mitigationSentence(alert({ mitigation: { acknowledged: true, acknowledgedBy: "ada", acknowledgedVia: "explicit" } })),
    ).toBe("Acknowledged by ada");
    expect(
      mitigationSentence(alert({ mitigation: { acknowledged: true, acknowledgedBy: "ada", acknowledgedVia: "action" } })),
    ).toBe("Being worked on by ada");
  });

  it("puts a standing silence above an acknowledgement", () => {
    const future = new Date(Date.now() + 3600_000).toISOString();
    const row = alert({
      mitigation: {
        acknowledged: true,
        acknowledgedBy: "ada",
        silencedBy: "grace",
        silencedUntil: future,
        silenceReason: "waiting on the upstream fix",
      },
    });
    expect(mitigationSentence(row)).toBe("Silenced by grace — waiting on the upstream fix");
  });

  it("names whoever took the escalated ticket", () => {
    expect(mitigationSentence(alert({ mitigation: { claimedBy: "ops" } }))).toBe("Claimed by ops");
  });
});

describe("silences", () => {
  // An expired silence is not a silence. The expiry is the whole reason one is
  // safe to grant, so a screen that went on rendering a lapsed one as quiet
  // would have given away the thing that made it safe.
  it("lapse", () => {
    const past = new Date(Date.now() - 1000).toISOString();
    const future = new Date(Date.now() + 3600_000).toISOString();
    expect(silenced(alert({ mitigation: { silencedUntil: past } }))).toBe(false);
    expect(silenced(alert({ mitigation: { silencedUntil: future } }))).toBe(true);
    expect(silenced(alert())).toBe(false);
  });

  // The form must not make the reader invent an expiry before it will let them
  // past, and the default has to be inside the bound the API enforces.
  it("are offered an end the API will accept", () => {
    const offered = new Date(defaultSilenceUntil(new Date("2026-04-01T09:00:00Z")));
    expect(offered.getTime()).toBeGreaterThan(new Date("2026-04-01T09:00:00Z").getTime());
    const days = (offered.getTime() - new Date("2026-04-01T09:00:00Z").getTime()) / 86_400_000;
    expect(days).toBeLessThanOrEqual(MAX_SILENCE_DAYS);
  });
});

describe("the headline", () => {
  it("counts what is asking for somebody, and not what is not", () => {
    expect(
      alertsSentence([alert({ tier: "page" }), alert({ tier: "ticket" }), alert({ tier: "log" })]),
    ).toBe("1 to act on now · 1 needing a fix");
  });

  // A list of nothing but logs is a list nothing is asking of anybody, which
  // is the sentence to say rather than "3 alerts".
  it("says so when nothing is", () => {
    expect(alertsSentence([alert({ tier: "log" })])).toBe("Nothing needs acting on.");
    expect(alertsSentence([])).toBe("Nothing needs acting on.");
  });
});

describe("audiences", () => {
  // The developer's row is the project's, and it is named for the project
  // rather than for the role: a screen saying "Developer" beside a row a
  // viewer is reading would be naming somebody else.
  it("are named for whose row it is", () => {
    expect(audienceLabel("developer")).toBe("Project");
    expect(audienceLabel("operator")).toBe("Operator");
  });
});
