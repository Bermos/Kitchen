import type { NotificationDelivery, NotificationSubscription } from "./api";

/**
 * Outbound notifications, as the dashboard reads them.
 *
 * The platform sends one shape of payload, signed, to an address somebody
 * chose — everything vendor-shaped (a chat app's card, a pager's incident) is
 * a small relay in front of it. So the screen has three jobs and this module
 * is the part of them that is not markup: name the events in the words a
 * person would use, say what state a subscription is in, and say what became
 * of one delivery.
 */

/** The vocabulary, in the order it is offered. It matches
 * `AllNotificationEvents` in the API, and the API refuses anything else. */
export const NOTIFICATION_EVENTS: { value: string; label: string; help: string }[] = [
  {
    value: "deploy.succeeded",
    label: "A deploy landed",
    help: "A release went live — promoted, deployed from a push, or rolled back to.",
  },
  { value: "build.failed", label: "A build failed", help: "A commit did not make it to an image." },
  {
    value: "environment.unhealthy",
    label: "An environment went unhealthy",
    help: "It stopped serving without anybody deploying anything.",
  },
  { value: "preview.created", label: "A preview was published", help: "A pull request got its own URL." },
  { value: "preview.destroyed", label: "A preview went away", help: "Its pull request was closed or merged." },
  { value: "alert.firing", label: "A saved query crossed its threshold", help: "An alert on a log query fired." },
];

/** What one event is called, falling back to the wire name — a subscription
 * written before the dashboard learned an event still has to render. */
export function eventLabel(value: string): string {
  return NOTIFICATION_EVENTS.find((event) => event.value === value)?.label ?? value;
}

/** The events of a subscription, as a sentence. */
export function eventSummary(subscription: NotificationSubscription): string {
  if (!subscription.events.length) return "nothing";
  return subscription.events.map(eventLabel).join(", ");
}

/** Whether a subscription is working, and what to say about it.
 *
 * Suspended comes first and is deliberately not an error: it is somebody
 * holding delivery while a receiver is repaired, and what is already queued is
 * waiting rather than lost.
 */
export function subscriptionState(subscription: NotificationSubscription): {
  tone: "success" | "warning" | "error";
  words: string;
} {
  if (subscription.suspended) return { tone: "warning", words: "paused — nothing new is queued" };
  if (!subscription.ready) return { tone: "error", words: subscription.reason || "not ready" };
  if (subscription.deadLettered > 0) {
    return {
      tone: "warning",
      words: `${subscription.deadLettered} never arrived`,
    };
  }
  if (subscription.lastResult === "failed") return { tone: "warning", words: "the last attempt failed" };
  return { tone: "success", words: subscription.delivered ? "delivering" : "nothing sent yet" };
}

/** One delivery's state, in the same three tones. */
export function deliveryTone(delivery: NotificationDelivery): "success" | "warning" | "error" {
  if (delivery.phase === "Delivered") return "success";
  return delivery.phase === "DeadLettered" ? "error" : "warning";
}

/** What became of one delivery, in a sentence rather than a phase. */
export function deliveryWords(delivery: NotificationDelivery): string {
  if (delivery.phase === "Delivered") return `accepted after ${attemptWords(delivery.attempts)}`;
  if (delivery.phase === "DeadLettered") {
    return `never arrived — ${delivery.lastError || `${attemptWords(delivery.attempts)} failed`}`;
  }
  if (delivery.attempts === 0) return "queued";
  return `${attemptWords(delivery.attempts)} so far — trying again`;
}

function attemptWords(attempts: number): string {
  return attempts === 1 ? "1 attempt" : `${attempts} attempts`;
}

/** The shortest signing key the API will store. A signature is worth exactly
 * what its key is worth. */
export const MIN_SECRET_LENGTH = 16;

/**
 * A signing key, generated here rather than by the platform.
 *
 * The API never reads a credential back, so a key it invented could only be
 * shown once — in a response that would then live in a browser's memory and
 * whatever logged it. Generating it in the page keeps the one copy where it is
 * needed: in front of the person who is about to paste it into their relay.
 */
export function generateSecret(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

/** Why this URL cannot be subscribed, or empty when it can be. The API refuses
 * the same things; this is so the form can say so before it is sent. */
export function urlProblem(raw: string): string {
  const url = raw.trim();
  if (!url) return "";
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return "That is not a URL.";
  }
  if (parsed.protocol !== "https:") {
    return "It must be https — a signed payload over plain HTTP is one anybody on the path can read.";
  }
  if (!parsed.host) return "It must be absolute, with a host.";
  return "";
}
