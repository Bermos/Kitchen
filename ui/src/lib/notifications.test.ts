import { describe, expect, it } from "vitest";
import type { NotificationDelivery, NotificationSubscription } from "./api";
import {
  MIN_SECRET_LENGTH,
  NOTIFICATION_EVENTS,
  deliveryTone,
  deliveryWords,
  eventLabel,
  eventSummary,
  generateSecret,
  subscriptionState,
  urlProblem,
} from "./notifications";

// The screen's job is to say whether somebody is actually being told, and
// these are the judgements behind that sentence.

function subscription(overrides: Partial<NotificationSubscription> = {}): NotificationSubscription {
  return {
    name: "shop-relay",
    url: "https://relay.example.com/kitchen",
    events: ["deploy.succeeded"],
    project: "shop",
    scope: "project",
    suspended: false,
    maxAttempts: 5,
    timeoutSeconds: 10,
    createdAt: "2026-09-01T09:00:00Z",
    ready: true,
    delivered: 12,
    failed: 0,
    deadLettered: 0,
    ...overrides,
  };
}

function delivery(overrides: Partial<NotificationDelivery> = {}): NotificationDelivery {
  return {
    name: "shop-relay-8f2c1",
    subscription: "shop-relay",
    event: "build.failed",
    eventId: "9f1c",
    phase: "Pending",
    attempts: 0,
    queuedAt: "2026-09-04T02:00:00Z",
    ...overrides,
  };
}

describe("the event vocabulary", () => {
  it("is the API's, and every entry says what it means", () => {
    // The API refuses anything else, so a checkbox the dashboard invented
    // would be a form that cannot be submitted.
    expect(NOTIFICATION_EVENTS.map((event) => event.value)).toEqual([
      "deploy.succeeded",
      "build.failed",
      "environment.unhealthy",
      "preview.created",
      "preview.destroyed",
      "alert.firing",
    ]);
    for (const event of NOTIFICATION_EVENTS) {
      expect(event.label.length, `${event.value} needs a label`).toBeGreaterThan(0);
      expect(event.help.length, `${event.value} needs a sentence`).toBeGreaterThan(0);
    }
  });

  it("falls back to the wire name for an event this dashboard predates", () => {
    expect(eventLabel("deploy.succeeded")).toBe("A deploy landed");
    expect(eventLabel("something.new")).toBe("something.new");
  });

  it("summarises what a subscription asked for", () => {
    expect(eventSummary(subscription({ events: ["deploy.succeeded", "build.failed"] }))).toBe(
      "A deploy landed, A build failed",
    );
    expect(eventSummary(subscription({ events: [] }))).toBe("nothing");
  });
});

describe("whether a subscription is working", () => {
  it("says so when it is", () => {
    expect(subscriptionState(subscription())).toEqual({ tone: "success", words: "delivering" });
    expect(subscriptionState(subscription({ delivered: 0 })).words).toBe("nothing sent yet");
  });

  it("puts paused before everything, and does not call it an error", () => {
    // Suspending is somebody holding delivery while a receiver is repaired:
    // what is queued waits rather than being lost.
    const paused = subscriptionState(subscription({ suspended: true, ready: false, reason: "whatever" }));
    expect(paused.tone).toBe("warning");
    expect(paused.words).toContain("paused");
  });

  it("shows the reconciler's reason when it cannot deliver at all", () => {
    const broken = subscriptionState(subscription({ ready: false, reason: "the signing secret is not there" }));
    expect(broken).toEqual({ tone: "error", words: "the signing secret is not there" });
  });

  it("does not call a subscription healthy while anything of its never arrived", () => {
    expect(subscriptionState(subscription({ deadLettered: 2 }))).toEqual({
      tone: "warning",
      words: "2 never arrived",
    });
    expect(subscriptionState(subscription({ lastResult: "failed" })).tone).toBe("warning");
  });
});

describe("what became of one delivery", () => {
  it("reads as a sentence rather than a phase", () => {
    expect(deliveryWords(delivery({ phase: "Delivered", attempts: 1 }))).toBe("accepted after 1 attempt");
    expect(deliveryWords(delivery({ phase: "Delivered", attempts: 3 }))).toBe("accepted after 3 attempts");
    expect(deliveryWords(delivery())).toBe("queued");
    expect(deliveryWords(delivery({ attempts: 2 }))).toContain("trying again");
    expect(
      deliveryWords(delivery({ phase: "DeadLettered", attempts: 5, lastError: "receiver answered 502" })),
    ).toBe("never arrived — receiver answered 502");
  });

  it("is the only one of the three that is an error", () => {
    expect(deliveryTone(delivery({ phase: "Delivered" }))).toBe("success");
    expect(deliveryTone(delivery())).toBe("warning");
    expect(deliveryTone(delivery({ phase: "DeadLettered" }))).toBe("error");
  });
});

describe("the signing key", () => {
  it("is generated here, long, and never the same twice", () => {
    const first = generateSecret();
    expect(first).toMatch(/^[0-9a-f]{64}$/);
    expect(first.length).toBeGreaterThanOrEqual(MIN_SECRET_LENGTH);
    expect(generateSecret()).not.toBe(first);
  });
});

describe("the address", () => {
  it("refuses what the API would refuse, before it is sent", () => {
    expect(urlProblem("https://relay.example.com/kitchen")).toBe("");
    // Empty is not a problem yet — it is a form nobody has finished.
    expect(urlProblem("")).toBe("");
    expect(urlProblem("relay.example.com")).toContain("not a URL");
    expect(urlProblem("http://relay.example.com")).toContain("https");
  });
});
