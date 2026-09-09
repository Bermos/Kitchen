# Kitchen — Custom domains

Every environment already has a generated URL. A custom domain is an address
somebody else owns, pointed at one.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Custom domains

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"hostname": "shop.example.com", "environment": "shop-production"}' \
  https://kitchen.apps.example.com/api/v1/domains
```

A domain is a hostname in a zone *you* control — names under the platform's
base domain are refused, because they are generated and routed already — and
the environment it should reach. `tls` is optional: `acme`, `cloudflared` or
`none`, inheriting the platform's mode when absent. The `name` defaults to
the hostname with dots turned into dashes. A hostname already attached is a
`409`; an environment that does not exist a `400`. So is an environment of a
project whose
[`exposure` is `internal`](projects.md#an-internal-project): nothing of such a
project is published, so there is no route for a custom hostname to ride, and
creating the Domain would be the one route that setting exists to prevent.

Answers `201`, but creating the object changes no traffic by itself: the
domain has to be **verified** first, and the next move is the caller's. `GET
/domains/{name}` (and the create response, once the reconciler has run)
carries `verification` — the exact TXT record and value to create, or the
CNAME that both proves ownership and points traffic at the platform. The
`Verified` condition says which of the real failure modes applies: record
absent, record present with the wrong value, or a lookup that failed.
`CertificateReady` and `RouteProgrammed` report the rest of the journey; in
`acme` mode issuance runs over HTTP-01 through the shared Gateway, so it
finishes only once the hostname resolves to the platform. While it is
`Issuing`, the message carries what cert-manager's own challenge says about
itself — `wrong status code '503', expected '200'` is the Gateway unable to
reach the solver, `404` is the hostname answered by something other than the
platform — so the reason a domain is stuck is on the domain rather than two
objects down in the cluster.

While it is finishing, the hostname is published **on port 80**, in cleartext.
That is not a gap in the design; it is what HTTP-01 requires of any host that
answers a challenge. The certificate cannot be issued until the challenge is
answered, the challenge is answered at this hostname on port 80, and the
per-domain HTTPS listener cannot exist until the certificate does — so port 80
is what the environment's route binds meanwhile. `RouteProgrammed` reports
`AwaitingCertificate` and says so.

The window closes by itself. The moment the secret exists the hostname moves to
its own HTTPS listener and joins the port-80 redirect's names, so cleartext
stops being served for it without anything having to be switched over. The
redirect covers only the names the platform already terminates TLS for, because
redirecting a name whose certificate is still being issued would send the ACME
validator to an HTTPS address that only completing that challenge can create.

### What each mode does on port 80

The mode decides which listener on the shared Gateway carries the hostname, and
therefore whether plain HTTP answers for it at all:

| `tls` | Port 80 | Port 443 |
| --- | --- | --- |
| `acme`, certificate issued | redirects to HTTPS | the domain's own listener, its own certificate |
| `acme`, awaiting certificate | **serves the environment**, and the HTTP-01 challenge | nothing — the listener does not exist yet |
| `cloudflared` | **serves the environment** | nothing at the Gateway; the tunnel terminates TLS at Cloudflare's edge |
| `none` | **serves the environment** | nothing |

So `cloudflared` and `none` publish on port 80 from the moment the domain is
verified and never stop, which is why a domain in either mode is reachable
without an issuance step at all. Under `cloudflared` that is not cleartext on
the internet — Cloudflare terminates TLS and the tunnel carries the request —
but at the Gateway it is the same plain-HTTP listener. Under `none` it is
cleartext end to end, which is the point of the mode and why it belongs on a
private network or behind something else that terminates TLS.

`RouteProgrammed` is also read by the environment's request endpoints. Once the
environment's route carries the hostname, anything short of `True` means the
platform's edge is what answers on it, so the traffic that arrives meanwhile is
not counted as what the environment served; see [what the internet asked of an
environment](environments.md#a-hostname-the-platform-is-not-publishing-yet-is-not-this-environments-traffic).

`DELETE /domains/{name}` answers `202`: the operator's finalizer still has
the domain's certificate and secret to remove, and the Gateway drops the
hostname as the reconcilers catch up. The DNS records in your zone are yours;
the platform never touches them.
