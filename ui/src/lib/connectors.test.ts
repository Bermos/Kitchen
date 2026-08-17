import { describe, expect, it } from "vitest";
import { instanceOrigin, providerGuidance, testSummary, testTone } from "./connectors";

describe("connectors", () => {
  it("tells a GitHub token what it has to be allowed to do, and where to make one", () => {
    const guidance = providerGuidance("github");
    expect(guidance?.purpose).toContain("webhook");
    // Both token flavours, named the way GitHub's own form names them.
    const permissions = guidance?.permissions.join(" ") ?? "";
    expect(permissions).toContain("Webhooks");
    expect(permissions).toContain("repo scope");
    // The link opens a form that is already correct.
    expect(guidance?.link?.href).toContain("settings/personal-access-tokens/new");
    expect(guidance?.link?.href).toContain("repository_hooks=write");
    expect(guidance?.link?.href).toContain("contents=read");
  });

  it("asks for the deploy-reporting permissions", () => {
    // The commit status, the deployment and the pull-request comment are each
    // posted with this token, and each needs its own permission.
    const guidance = providerGuidance("github");
    for (const permission of ["statuses=write", "deployments=write", "pull_requests=write"]) {
      expect(guidance?.link?.href).toContain(permission);
    }
    // Short of them the platform still deploys; it just goes unannounced.
    expect(guidance?.permissions.join(" ")).toContain("still builds and deploys");
  });

  it("points a self-hosted GitHub at its own token page", () => {
    const guidance = providerGuidance("github", "https://github.internal/api/v3");
    expect(guidance?.link?.href).toBe("https://github.internal/settings/tokens");
  });

  it("says outright that gitlab and gitea are not implemented", () => {
    expect(providerGuidance("gitlab")?.purpose).toContain("no GitLab implementation yet");
    expect(providerGuidance("gitea")?.purpose).toContain("no Gitea implementation yet");
    expect(providerGuidance("svn")).toBeUndefined();
  });

  it("reads an API URL down to the instance it names", () => {
    expect(instanceOrigin("https://github.internal/api/v3")).toBe("https://github.internal");
    expect(instanceOrigin("  https://ghe.example.com:8443/api/v3  ")).toBe("https://ghe.example.com:8443");
    expect(instanceOrigin("github.internal")).toBeUndefined();
    expect(instanceOrigin("")).toBeUndefined();
    expect(instanceOrigin(undefined)).toBeUndefined();
  });

  it("keeps a provider that is down from reading as a credential that is wrong", () => {
    const verdict = (reachable: boolean, credentialChecked: boolean, credentialValid: boolean) => ({
      reachable,
      credentialChecked,
      credentialValid,
    });
    expect(testTone(verdict(true, true, true))).toBe("success");
    expect(testTone(verdict(true, true, false))).toBe("error");
    expect(testTone(verdict(true, false, false))).toBe("warning");
    expect(testTone(verdict(false, false, false))).toBe("warning");

    expect(testSummary(verdict(true, true, true))).toContain("accepted");
    expect(testSummary(verdict(true, true, false))).toContain("rejected");
    expect(testSummary(verdict(true, false, false))).toContain("did not check");
    expect(testSummary(verdict(false, false, false))).toContain("could not be reached");
  });
});
