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
`409`; an environment that does not exist a `400`.

Answers `201`, but creating the object changes no traffic by itself: the
domain has to be **verified** first, and the next move is the caller's. `GET
/domains/{name}` (and the create response, once the reconciler has run)
carries `verification` — the exact TXT record and value to create, or the
CNAME that both proves ownership and points traffic at the platform. The
`Verified` condition says which of the real failure modes applies: record
absent, record present with the wrong value, or a lookup that failed.
`CertificateReady` and `RouteProgrammed` report the rest of the journey; in
`acme` mode issuance runs over HTTP-01 through the shared Gateway, so it
finishes only once the hostname resolves to the platform.

`DELETE /domains/{name}` answers `202`: the operator's finalizer still has
the domain's certificate and secret to remove, and the Gateway drops the
hostname as the reconcilers catch up. The DNS records in your zone are yours;
the platform never touches them.
