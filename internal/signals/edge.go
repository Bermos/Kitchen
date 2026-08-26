/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package signals

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/flows"
)

// The edge-and-certificates table of §7: the path between the internet and the
// application, every hop of which can be broken while everything behind it
// reads healthy.

const (
	SignalGatewayUnprogrammed ID = "gateway.unprogrammed"
	SignalRouteRejected       ID = "route.rejected"
	SignalDNSMismatch         ID = "dns.mismatch"
	SignalCertExpiring        ID = "cert.expiring"
	SignalTunnelDown          ID = "tunnel.down"
	SignalUnroutedHosts       ID = "edge.unrouted-hosts"
)

func edgeSignals() []Signal {
	return []Signal{{
		ID:       SignalGatewayUnprogrammed,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "the shared Gateway is not programmed, so nothing published is reachable",
		Requires: []Input{InputGateways},
		Evaluate: evaluateGatewayUnprogrammed,
	}, {
		ID:      SignalRouteRejected,
		Version: 1,
		// Deliberately developer, where §7 lists it under an operator table.
		// An application's route carries the project and environment labels, so
		// routeScope attributes the finding to the environment whose URL is
		// answering 404 — and ResolvedRefs=False is the half a developer
		// causes. Audience now drives ForEnvironment, so this line is what puts
		// it on that environment's diagnostics strip; the operator sees it
		// either way, since developer findings are additive.
		Audience: AudienceDeveloper,
		Summary:  "an HTTPRoute was not accepted, or its backends did not resolve",
		Requires: []Input{InputRoutes},
		Evaluate: evaluateRouteRejected,
	}, {
		ID: SignalDNSMismatch,
		// 2: the comparison is against the address the *internet* reaches this
		// platform at, which is the Gateway's own only where that address is
		// globally routable. Under version 1 every install whose Gateway sat
		// behind a router's port forward reported each of its published
		// hostnames as a critical, permanently.
		Version:  2,
		Audience: AudienceOperator,
		Summary:  "a published name does not resolve to the address the platform is reached at",
		Requires: []Input{InputDNS},
		Evaluate: evaluateDNSMismatch,
	}, {
		ID:       SignalCertExpiring,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "a certificate is inside its renewal window with the renewal not progressing",
		Requires: []Input{InputCertificates},
		Evaluate: evaluateCertExpiring,
	}, {
		ID:       SignalTunnelDown,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "cloudflared is unavailable or flapping",
		Requires: []Input{InputWorkloads},
		Evaluate: evaluateTunnelDown,
	}, {
		ID: SignalUnroutedHosts,
		// 2: the hostnames the platform's own routes publish are subtracted
		// before a host counts as unrouted, which is what stops the dashboard's
		// own URL being reported as traffic nobody published.
		Version:  2,
		Audience: AudienceOperator,
		Summary:  "the edge is being asked, persistently, for hosts the platform never published",
		// The raw rows, not the rollup: the unrouted bucket is a group-by over
		// http_requests, and it is the only rule in the catalogue that reads it.
		//
		// The routes are required beside them because the store's bucket is
		// "not attributed to a project", which is a wider set than "not
		// published": the dashboard, the API and the identity provider are
		// served by routes carrying no project, so their traffic lands there
		// too. Without the routes the rule would report the platform's own URL
		// as a host nobody published, and a listing that failed must make it
		// say so rather than accuse.
		Requires: []Input{InputRawRequests, InputRoutes},
		Evaluate: evaluateUnroutedHosts,
	}}
}

// evaluateGatewayUnprogrammed catches the install that is green everywhere and
// reachable nowhere.
//
// The case worth naming is AddressNotAssigned. cloudflared removes the need
// for the LoadBalancer address to be *routable*, not the need for it to exist:
// Cilium reports Programmed=False without one, and the platform never goes
// ready. That has happened on bare metal, and the reader whose tunnel is up
// has every reason to assume the address does not matter.
func evaluateGatewayUnprogrammed(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for i := range snapshot.Gateways {
		gateway := &snapshot.Gateways[i]
		condition := findCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		if condition == nil || condition.Status == metav1.ConditionTrue {
			continue
		}
		scope := Scope{Kind: ScopePlatform, Namespace: gateway.Namespace, Name: gateway.Name}
		findings = append(findings, fire(SignalGatewayUnprogrammed, SeverityCritical, scope,
			condition.LastTransitionTime.Time,
			"the edge is not programmed",
			sentence(
				withReason(condition.Reason, condition.Message),
				addressSuspect(condition.Reason, snapshot),
				"no published URL resolves to anything while this is true, whatever the applications "+
					"behind it report",
			),
			EvidencePlatformEdge))
	}
	return findings
}

// addressSuspect explains the address case, which is the one a person cannot
// diagnose from the condition alone.
func addressSuspect(reason string, snapshot *Snapshot) string {
	if !strings.Contains(reason, "Address") {
		return ""
	}
	suspect := "the Gateway has no LoadBalancer address: Cilium needs one assigned before it will " +
		"program the listeners"
	if snapshot.Platform.CloudflaredEnabled {
		suspect += ", and cloudflared does not remove that need — it only removes the need for the " +
			"address to be routable"
	}
	return suspect
}

func evaluateRouteRejected(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for i := range snapshot.Routes {
		route := &snapshot.Routes[i]
		reason := routeTrouble(route)
		if reason == "" {
			continue
		}
		scope := routeScope(route)
		findings = append(findings, fire(SignalRouteRejected, SeverityCritical, scope, snapshot.Now,
			"the route is not serving",
			sentence(
				reason,
				fmt.Sprintf("HTTPRoute %s in namespace %s, for %s",
					route.Name, route.Namespace, hostnamesOf(route)),
				"the Gateway will answer these hostnames with a 404 rather than reaching the "+
					"application",
			),
			scopeEvidence(scope, sectionRequests)))
	}
	return findings
}

// routeTrouble reads a route's parent statuses for the first thing wrong, or
// "" when every parent accepted it and resolved its backends.
//
// Both conditions matter and they fail differently: Accepted=False is the
// Gateway refusing the attachment (a hostname outside the listener, a section
// that does not exist), ResolvedRefs=False is the route attached to a backend
// that is not there. The second is the one a developer causes.
func routeTrouble(route *gatewayv1.HTTPRoute) string {
	for i := range route.Status.Parents {
		parent := &route.Status.Parents[i]
		for _, conditionType := range []string{
			string(gatewayv1.RouteConditionAccepted),
			string(gatewayv1.RouteConditionResolvedRefs),
		} {
			condition := findCondition(parent.Conditions, conditionType)
			if condition == nil || condition.Status == metav1.ConditionTrue {
				continue
			}
			return fmt.Sprintf("%s=%s from %s — %s", conditionType, condition.Status,
				parent.ControllerName, withReason(condition.Reason, condition.Message))
		}
	}
	return ""
}

func routeScope(route *gatewayv1.HTTPRoute) Scope {
	project := route.Labels[controller.LabelProject]
	environment := route.Labels[controller.LabelEnvironment]
	if project != "" && environment != "" {
		return Scope{Kind: ScopeEnvironment, Project: project, Environment: environment}
	}
	return Scope{Kind: ScopeWorkload, Namespace: route.Namespace, Name: route.Name}
}

func hostnamesOf(route *gatewayv1.HTTPRoute) string {
	if len(route.Spec.Hostnames) == 0 {
		return "every hostname the listener serves"
	}
	names := make([]string, 0, len(route.Spec.Hostnames))
	for _, hostname := range route.Spec.Hostnames {
		names = append(names, string(hostname))
	}
	return strings.Join(names, ", ")
}

// evaluateDNSMismatch is the "everything green, nothing reachable" detector.
//
// The rule itself is trivial — a name resolved somewhere other than the edge,
// or did not resolve at all. What matters is where it does *not* fire, and
// most of that is decided before the rule runs: the gatherer marks the DNS
// input not-applicable when the platform is behind cloudflared (names point at
// Cloudflare by design) or when there is no address of any kind to probe on
// behalf of, and unreadable when the resolver itself misbehaved. A broken
// resolver must never look like broken DNS.
//
// The last of it is decided here, and it is the case the rule originally got
// wrong. The Gateway's address is where traffic arrives *inside* the cluster,
// which is not where it arrives from the internet: on bare metal a router
// commonly forwards :80 and :443 from a public address to a Gateway programmed
// with an RFC1918 one, and a public record naming 10.0.10.240 would then be
// the fault rather than the fix. So a name is compared only against an address
// the internet could plausibly answer with — the declared public addresses, or
// a Gateway address that is itself globally routable. Where there is no such
// address the platform does not know what its records ought to say, and
// declines to guess: see expectedDNSAddresses.
func evaluateDNSMismatch(snapshot *Snapshot) []Finding {
	expected := expectedDNSAddresses(snapshot.Platform)
	accepted := acceptedDNSAddresses(snapshot.Platform, expected)
	findings := make([]Finding, 0, 1)
	for _, probe := range snapshot.DNS {
		switch {
		case !probe.Exists:
			// A name the platform published that resolves to nothing at all is
			// broken under every topology, including the ones with nothing to
			// compare a resolved address against.
		case len(expected) == 0:
			continue
		case containsAny(probe.Addresses, accepted):
			continue
		}
		scope := Scope{Kind: ScopeDomain, Name: probe.Host}
		findings = append(findings, fire(SignalDNSMismatch, SeverityCritical, scope, snapshot.Now,
			dnsMismatchTitle(probe),
			sentence(
				dnsMismatchHeadline(probe, expected),
				dnsMismatchExplanation(probe),
			),
			hostEvidence(probe.Host)))
	}
	return findings
}

// expectedDNSAddresses is what a published name ought to resolve to, and it is
// empty when the platform cannot know.
//
// A declared public address is the answer whenever there is one: it is the
// outside of a translation nothing in the cluster can observe, and an operator
// who wrote it down is telling the platform what its records should say. With
// none declared, the Gateway's own address is that answer only if the internet
// could reach it — an address in an RFC1918 range, in the carrier-grade NAT
// range, on the loopback or link-local, or a hostname rather than an address,
// is one no public record should name, so there is nothing here to check a
// record against and the rule confines itself to records that are missing
// outright.
func expectedDNSAddresses(platform PlatformFacts) []string {
	if len(platform.PublicAddresses) > 0 {
		return platform.PublicAddresses
	}
	if publiclyRoutable(platform.GatewayAddress) {
		return []string{platform.GatewayAddress}
	}
	return nil
}

// acceptedDNSAddresses is the wider set the rule stays quiet about: what a
// name should say, plus the Gateway's own address.
//
// The addition covers split horizon, which is the common companion of the
// translation publicAddresses exists for. The operator resolves names from
// inside the cluster, where a resolver may well answer with the private
// address that traffic actually lands on — the same correct configuration seen
// from the other side of the router, and not something to report as a
// misdirected record.
func acceptedDNSAddresses(platform PlatformFacts, expected []string) []string {
	if platform.GatewayAddress == "" || containsString(expected, platform.GatewayAddress) {
		return expected
	}
	return append(append(make([]string, 0, len(expected)+1), expected...), platform.GatewayAddress)
}

// publiclyRoutable tells an address the internet can reach from one it cannot.
// Anything that is not an address at all — a Gateway addressed by hostname —
// answers false, since a resolved address can never equal it.
func publiclyRoutable(address string) bool {
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	return !addr.IsPrivate() && !addr.IsLoopback() && !addr.IsUnspecified() &&
		!addr.IsLinkLocalUnicast() && !addr.IsLinkLocalMulticast() &&
		!addr.IsInterfaceLocalMulticast() && !carrierGradeNAT.Contains(addr)
}

// carrierGradeNAT is RFC 6598, which netip's IsPrivate does not cover and
// which every address behind an ISP's own NAT sits in.
var carrierGradeNAT = netip.MustParsePrefix("100.64.0.0/10")

func dnsMismatchTitle(probe DNSProbe) string {
	if !probe.Exists {
		return "published name does not resolve"
	}
	return "published name does not point here"
}

func dnsMismatchHeadline(probe DNSProbe, expected []string) string {
	if !probe.Exists {
		if len(expected) == 0 {
			return fmt.Sprintf("%s has no address record at all, and the platform published it",
				probe.Host)
		}
		return fmt.Sprintf("%s does not resolve, and this platform is reached at %s",
			probe.Host, strings.Join(expected, ", "))
	}
	return fmt.Sprintf("%s resolves to %s, not to %s", probe.Host,
		strings.Join(probe.Addresses, ", "), strings.Join(expected, ", "))
}

func dnsMismatchExplanation(probe DNSProbe) string {
	if !probe.Exists {
		return "the platform published this hostname and the cluster is serving it; nothing " +
			"answers for the name, so visitors never reach the edge at all"
	}
	return "the platform published this hostname and the cluster is serving it; the record in " +
		"front of it names somewhere else, so visitors never reach the edge at all — if that " +
		"address does forward here, it belongs in spec.ingress.publicAddresses"
}

// evaluateCertExpiring reports a certificate running out with nothing being
// done about it.
//
// Approaching expiry is not by itself news: cert-manager renews at two thirds
// of the lifetime and says nothing while it works. The finding is the
// combination — inside the window *and* the renewal is not progressing — and
// the ACME order's own error is attached verbatim, because it is the only
// string that says whether the problem is the DNS token, the rate limit or the
// solver.
func evaluateCertExpiring(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	certificates := append([]Certificate(nil), snapshot.Certificates...)
	sort.Slice(certificates, func(i, j int) bool {
		if certificates[i].Namespace != certificates[j].Namespace {
			return certificates[i].Namespace < certificates[j].Namespace
		}
		return certificates[i].Name < certificates[j].Name
	})

	for _, certificate := range certificates {
		remaining, firing := certificateTrouble(certificate, snapshot.Now)
		if !firing {
			continue
		}
		scope := Scope{Kind: ScopeDomain, Namespace: certificate.Namespace, Name: certificate.Name}
		findings = append(findings, fire(SignalCertExpiring, SeverityCritical, scope, snapshot.Now,
			certificateTitle(certificate, remaining),
			sentence(
				certificateHeadline(certificate, remaining),
				strings.TrimSpace(certificate.IssuingMessage+" "+certificate.Message),
				"wildcard certificates are DNS-01 only, so a failing renewal is almost always the "+
					"DNS solver's credentials rather than reachability",
			),
			hostEvidence(firstOr(certificate.DNSNames, certificate.Name))))
	}
	return findings
}

// certificateTrouble decides whether a certificate is in trouble and how long
// it has left.
//
// A certificate that has never been issued is in trouble immediately — there
// is no expiry to be near, and the Gateway's HTTPS listener is referencing a
// Secret that does not exist. One that is issued and inside the window is in
// trouble only if it is not ready, because a ready certificate inside its
// window is one cert-manager is about to renew.
func certificateTrouble(certificate Certificate, now time.Time) (time.Duration, bool) {
	if certificate.NotAfter.IsZero() {
		return 0, !certificate.Ready
	}
	remaining := certificate.NotAfter.Sub(now)
	if remaining > CertExpiryWindow {
		return remaining, false
	}
	// Inside the window. Ready means the current certificate is still valid,
	// which is only reassuring if a renewal is actually moving — and a stuck
	// renewal reports itself on the Issuing condition while Ready stays true.
	return remaining, !certificate.Ready || certificate.IssuingMessage != ""
}

func certificateTitle(certificate Certificate, remaining time.Duration) string {
	if certificate.NotAfter.IsZero() {
		return "certificate has never been issued"
	}
	if remaining <= 0 {
		return "certificate has expired"
	}
	return "certificate expires in " + duration(remaining)
}

func certificateHeadline(certificate Certificate, remaining time.Duration) string {
	names := strings.Join(certificate.DNSNames, ", ")
	if names == "" {
		names = certificate.Name
	}
	if certificate.NotAfter.IsZero() {
		return fmt.Sprintf("%s has no certificate at all: %s", names,
			withReason(certificate.Reason, certificate.Message))
	}
	return fmt.Sprintf("%s, %s left, renewal not progressing", names, duration(remaining))
}

// evaluateTunnelDown reads the cloudflared Deployment the Kitchen reconciler
// creates. It is not folded into the component survey because the tunnel is
// the whole path in for an installation that has one: unavailable cloudflared
// is not one component of many, it is the platform being unreachable.
func evaluateTunnelDown(snapshot *Snapshot) []Finding {
	deployment := findDeployment(snapshot, controller.PlatformNamespace, cloudflaredDeploymentName)
	if deployment == nil {
		// No tunnel configured. Nothing to be down.
		return nil
	}
	desired := replicasOrOne(deployment.Spec.Replicas)
	restarts := tunnelRestarts(snapshot)
	unavailable := deployment.Status.AvailableReplicas < desired
	flapping := restarts >= TunnelRestartsFiring
	if !unavailable && !flapping {
		return nil
	}

	scope := Scope{Kind: ScopePlatform, Name: "cloudflared"}
	return []Finding{fire(SignalTunnelDown, SeverityCritical, scope, snapshot.Now,
		tunnelTitle(unavailable),
		sentence(
			fmt.Sprintf("%d of %d replicas available, %s in %s",
				deployment.Status.AvailableReplicas, desired,
				plural(restarts, "restart", "restarts"), duration(RestartWindow)),
			"the tunnel is the only way in when it is enabled, so the Gateway being healthy behind "+
				"it changes nothing",
		),
		EvidencePlatformEdge)}
}

func tunnelTitle(unavailable bool) string {
	if unavailable {
		return "the tunnel is down"
	}
	return "the tunnel is flapping"
}

// tunnelRestarts counts restarts across cloudflared's pods, which is what
// tells "down" from "cannot hold a connection".
func tunnelRestarts(snapshot *Snapshot) int {
	restarts := 0
	for i := range snapshot.Pods {
		pod := &snapshot.Pods[i]
		if pod.Namespace != controller.PlatformNamespace ||
			pod.Labels[labelComponentKind] != cloudflaredComponent {
			continue
		}
		for _, status := range containerStatuses(pod) {
			restarts += int(status.RestartCount)
		}
	}
	return restarts
}

// evaluateUnroutedHosts reports the hosts the edge is being asked for that the
// platform never published.
//
// The store's bucket is not that set, and the difference is the platform
// itself. A request row is attributed by looking its host up in the routes
// that carry a project, so the dashboard, the API and the identity provider —
// published by routes that carry none, since their traffic belongs to no
// project — land in the same unattributed bucket a stale DNS record does. The
// routes are the operator's own knowledge of what it published, so the rule
// subtracts them here: whatever any route names, whoever published it, is not
// a host nobody published.
//
// The sustain check is what separates the two remaining causes. A scanner
// walking the address space asks once and goes away; a name that was published
// and then stopped being — a custom domain whose Domain object was deleted
// while its record was not — keeps asking for as long as its users do, and
// those are real people getting a 404.
func evaluateUnroutedHosts(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	published := flows.PublishedHosts(snapshot.Routes)
	hosts := append([]clickhouse.UnroutedHost(nil), snapshot.UnroutedHosts...)
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Host < hosts[j].Host })

	for _, host := range hosts {
		span := host.LastSeen.Sub(host.FirstSeen)
		if host.Requests < UnroutedMinRequests || span < UnroutedSustained {
			continue
		}
		if published.Covers(host.Host) {
			// Published, and merely unattributed: one of the platform's own
			// surfaces, or an environment whose route the follower had not
			// read yet when the traffic arrived.
			continue
		}
		scope := Scope{Kind: ScopeDomain, Name: host.Host}
		findings = append(findings, fire(SignalUnroutedHosts, SeverityWarning, scope, host.FirstSeen,
			"traffic for a host nobody published",
			sentence(
				fmt.Sprintf("%d requests for %s over %s", host.Requests, host.Host, duration(span)),
				"the platform never published this hostname, so the edge cannot attribute the "+
					"traffic to any environment",
				"a stale DNS record still pointing here, or a custom domain whose Domain object "+
					"was removed while its record was not",
			),
			hostEvidence(host.Host)))
	}
	return findings
}

// cloudflaredDeploymentName and cloudflaredComponent are the names the Kitchen
// reconciler gives the tunnel. They are spelled here because they are
// unexported there; kitchen_controller.go's are the definitions.
const (
	cloudflaredDeploymentName = "kitchen-cloudflared"
	cloudflaredComponent      = "cloudflared"
)

func findDeployment(snapshot *Snapshot, namespace, name string) *appsv1.Deployment {
	for i := range snapshot.Deployments {
		deployment := &snapshot.Deployments[i]
		if deployment.Namespace == namespace && deployment.Name == name {
			return deployment
		}
	}
	return nil
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// containsAny is containsString over a set of acceptable answers: a name is
// pointed here if any address it resolved to is one of them.
func containsAny(values, wanted []string) bool {
	for _, value := range values {
		if containsString(wanted, value) {
			return true
		}
	}
	return false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func firstOr(values []string, fallback string) string {
	if len(values) > 0 && values[0] != "" {
		return values[0]
	}
	return fallback
}
