/**
 * What the audit screen tells a reader about the anchor.
 *
 * The sentence is pinned here rather than read off the screen because it is
 * the whole of what the dashboard says about whether the log in front of the
 * reader is bounded by anything at all (#428).
 */
import { describe, expect, it } from "vitest";
import type { AuditVerification } from "./api";
import { anchorNote } from "./audit";

const sound: AuditVerification = {
  from: 1,
  to: 4,
  checked: 4,
  intact: true,
  findings: [],
  anchorPresent: true,
  anchor: 4,
  anchorOrigin: "genesis",
  truncated: false,
};

describe("anchorNote", () => {
  it("says nothing when there is no verification to describe", () => {
    expect(anchorNote(null)).toBeNull();
    expect(anchorNote(undefined)).toBeNull();
  });

  it("names the anchor a sound chain was checked against", () => {
    const note = anchorNote(sound);
    expect(note?.bad).toBe(false);
    expect(note?.text).toContain("ends at 4");
  });

  // The fault itself: a missing anchor arrived as `0` and the screen rendered
  // it as no gap at all, next to the line saying every record re-derives.
  it("reports a missing anchor as a finding rather than as sequence 0", () => {
    const note = anchorNote({
      ...sound,
      intact: false,
      anchorPresent: false,
      anchor: null,
      anchorOrigin: undefined,
      anchorMessage: "the head object outside the table is not there",
      findings: [{ sequence: 4, break: "unanchored", detail: "there is no anchor" }],
    });
    expect(note?.bad).toBe(true);
    expect(note?.text).toContain("There is no anchor");
    expect(note?.text).not.toContain("ends at 0");
    // A deleted object and a cluster that did not answer are the same gap and
    // very different events, so the reason is carried through.
    expect(note?.text).toContain("the head object outside the table is not there");
    expect(
      anchorNote({ ...sound, anchorPresent: false, anchor: null, anchorMessage: "the cluster did not answer" })?.text,
    ).toContain("the cluster did not answer");
  });

  // An adopted anchor is not a break, but it is a weaker claim, and it says
  // where the line falls.
  it("says where an adopted anchor's numbering was taken from", () => {
    const note = anchorNote({ ...sound, anchorOrigin: "adopted", anchorAdoptedFrom: 2 });
    expect(note?.bad).toBe(false);
    expect(note?.text).toContain("sequence 2");
    expect(note?.text).toContain("bounded by the hash chain alone");
  });
});
