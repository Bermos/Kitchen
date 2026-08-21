import { describe, expect, it } from "vitest";
import type { Environment, Promotion, PromotionStage } from "./api";
import {
  blockedPromotionFor,
  latestPromotionFor,
  newestFirst,
  pipelineShown,
  promotionTone,
  stageRows,
} from "./promotions";

// The pipeline section reasons over three lists the API already serves; the
// reasoning lives here so the screen stays a rendering of it.

const promo = (over: Partial<Promotion>): Promotion => ({
  name: "shop-promo-1",
  project: "shop",
  environment: "shop-staging",
  release: "shop-rel-1",
  requestedBy: "system:controller/build",
  trigger: "automatic",
  phase: "Applied",
  createdAt: "2026-08-20T10:00:00Z",
  ...over,
});

const env = (name: string, release: string): Environment =>
  ({ name, project: "shop", type: "production", release, createdAt: "2026-08-01T00:00:00Z" }) as Environment;

const stages: PromotionStage[] = [
  { name: "staging", environment: "shop-staging", autoPromote: false },
  { name: "production", environment: "shop-production", autoPromote: true },
];

describe("newestFirst", () => {
  it("sorts by creation time, names breaking the tie", () => {
    const sorted = newestFirst([
      promo({ name: "a", createdAt: "2026-08-20T10:00:00Z" }),
      promo({ name: "c", createdAt: "2026-08-20T11:00:00Z" }),
      promo({ name: "b", createdAt: "2026-08-20T11:00:00Z" }),
    ]);
    expect(sorted.map((p) => p.name)).toEqual(["c", "b", "a"]);
  });
});

describe("latestPromotionFor", () => {
  it("answers the newest promotion into the named environment", () => {
    const promotions = [
      promo({ name: "old", environment: "shop-production", createdAt: "2026-08-19T10:00:00Z" }),
      promo({ name: "new", environment: "shop-production", createdAt: "2026-08-20T10:00:00Z" }),
      promo({ name: "elsewhere", environment: "shop-staging", createdAt: "2026-08-21T10:00:00Z" }),
    ];
    expect(latestPromotionFor(promotions, "shop-production")?.name).toBe("new");
    expect(latestPromotionFor(promotions, "shop-qa")).toBeUndefined();
  });
});

describe("blockedPromotionFor", () => {
  it("alarms only when the newest promotion is the blocked one", () => {
    const superseded = [
      promo({ name: "blocked", phase: "Blocked", createdAt: "2026-08-19T10:00:00Z" }),
      promo({ name: "applied", phase: "Applied", createdAt: "2026-08-20T10:00:00Z" }),
    ];
    expect(blockedPromotionFor(superseded, "shop-staging")).toBeUndefined();

    const standing = [
      promo({ name: "applied", phase: "Applied", createdAt: "2026-08-19T10:00:00Z" }),
      promo({
        name: "blocked",
        phase: "Blocked",
        unmetRules: ["require-sbom"],
        createdAt: "2026-08-20T10:00:00Z",
      }),
    ];
    expect(blockedPromotionFor(standing, "shop-staging")?.name).toBe("blocked");
  });
});

describe("stageRows", () => {
  it("pairs each stage with its environment and newest promotion", () => {
    const rows = stageRows(
      stages,
      [env("shop-staging", "shop-rel-2")],
      [promo({ environment: "shop-staging", release: "shop-rel-2" })],
    );
    expect(rows).toHaveLength(2);
    expect(rows[0].release).toBe("shop-rel-2");
    expect(rows[0].promotion?.release).toBe("shop-rel-2");
    // A stage whose environment does not exist yet — no build has targeted
    // it — draws empty rather than being dropped: the rung is configured.
    expect(rows[1].environment).toBeUndefined();
    expect(rows[1].release).toBe("");
  });
});

describe("promotionTone", () => {
  it("maps the terminal phases to their alarm level", () => {
    expect(promotionTone("Applied")).toBe("success");
    expect(promotionTone("Blocked")).toBe("error");
    expect(promotionTone("Failed")).toBe("error");
    expect(promotionTone("AllowedWithException")).toBe("warning");
    expect(promotionTone("Pending")).toBe("neutral");
  });
});

describe("pipelineShown", () => {
  it("earns its place with stages or promotions, not otherwise", () => {
    expect(pipelineShown(undefined, [])).toBe(false);
    expect(pipelineShown([], [])).toBe(false);
    expect(pipelineShown(stages, [])).toBe(true);
    expect(pipelineShown(undefined, [promo({})])).toBe(true);
  });
});
