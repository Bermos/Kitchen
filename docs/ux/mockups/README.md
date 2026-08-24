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
