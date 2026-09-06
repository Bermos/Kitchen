/**
 * What the audit screen says about the chain's anchor.
 *
 * The anchor is the head object outside the table, and it is the only thing
 * that bounds a log rewritten from the end: a truncated chain rehashes
 * perfectly, so the records can never show their own tail being cut. The
 * screen used to work the gap out for itself — `Math.max(0, anchor - to)` —
 * which meant a missing anchor arrived as `0` and rendered as *no gap at all*,
 * next to the green line saying every record re-derives (#428).
 *
 * The comparison now lives in the API, where every reader of
 * `GET /api/v1/audit/verify` inherits it: a run that ends below the anchor is
 * a `truncated` finding and a run with no anchor is an `unanchored` one, and
 * both make `intact` false. What is left here is the sentence underneath —
 * which anchor the verdict was reached against, and whether that is something
 * to worry about — and it is in this file rather than in the view so that the
 * wording is pinned by a test like every other claim the dashboard makes.
 */
import type { AuditVerification } from "./api";

/** One line under the verdict, and whether it is bad news. */
export interface AnchorNote {
  /** True when the sentence is a finding rather than a statement of fact:
   *  there is nothing bounding this log, which is the case #428 is about. */
  bad: boolean;
  text: string;
}

/**
 * The sentence to print under a verification, or null when there is no
 * verification to describe.
 *
 * An adopted anchor is not a break — an installation upgrading from before the
 * head object existed has one legitimately — but it is a weaker claim than an
 * anchor that has been there since the chain started, and the reader is
 * entitled to know which they have and where the line falls.
 */
export function anchorNote(result: AuditVerification | null | undefined): AnchorNote | null {
  if (!result) return null;
  if (!result.anchorPresent) {
    const why = result.anchorMessage ?? "the head object outside the table is not there";
    return {
      bad: true,
      text: `There is no anchor: ${why}. A log cut off at the end would rehash perfectly, so nothing here would show it.`,
    };
  }
  if (result.anchorOrigin === "adopted") {
    return {
      bad: false,
      text: `The anchor outside the table says the chain ends at ${result.anchor}. It was adopted from the log's own last record, sequence ${result.anchorAdoptedFrom}: records up to there are bounded by the hash chain alone.`,
    };
  }
  return {
    bad: false,
    text: `The anchor outside the table says the chain ends at ${result.anchor}.`,
  };
}
