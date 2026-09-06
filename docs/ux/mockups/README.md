# UX review mockups

Mockups from a UX review of `ui/`, referenced by the issues they belong to. They are
design proposals, **not** screenshots of shipped code — nothing here renders from the
dashboard as it currently stands.

| File | Issue | Shows |
| --- | --- | --- |
| `1a-incident-band.png` | Overview: a failing project is a red dot, not a triage path | A **Needs attention** band above the project table, carrying the untruncated error, the blast radius and the resolving action |
| `1b-rollback-diff.png` | Rollback asks for trust it has not earned | Step 2 of rollback — live-vs-target release, image digest, env-var diff and the commits that stop being served |
| `1b-rollback-verify.png` | Rollback asks for trust it has not earned | Step 3 of rollback — replicas, route programming, 5xx and p95 after the swap |
| `1c-freshness.png` | Five invisible pollers, no indication of how old the screen is | One freshness control per screen: visible age, pause-while-I-read, and an explicit stale state |
| `4d-four-scopes.png` | The dashboard mixes four audiences in one flat navigation | Fleet, Project, Platform and Compliance as four scopes, each with its own root and its own place in the URL |
| `4e-project-shell.png` | The dashboard mixes four audiences in one flat navigation | The shell a project screen renders in, and the picker a developer screen opens with no project selected |
| `4a-audience-matrix.png` | An alert's tier belongs to the *pair* (condition, audience) | The table that turns a condition plus a reader into one of three delivery tiers |
| `4b-correlation-ladder.png` | Correlation should be a confidence ladder | Coincidence, shared dependency and shared change as three rungs, each naming what it does not know |
| `4c-policy-presets.png` | Correlation should be a confidence ladder | `/platform/policy`: the correlation and escalation numbers, with strict, balanced and homelab presets |
