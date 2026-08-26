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
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The condition Cilium raises when the Gateway has no LoadBalancer address,
// which cloudflared does not remove the need for.
const reasonAddressNotAssigned = "AddressNotAssigned"

func unprogrammedGateway(reason, message string) gatewayv1.Gateway {
	return gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.SharedGatewayName,
			Namespace: controller.PlatformNamespace,
		},
		Status: gatewayv1.GatewayStatus{Conditions: []metav1.Condition{{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.NewTime(testNow.Add(-time.Hour)),
		}}},
	}
}

func TestGatewayUnprogrammedNamesTheAddress(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Gateways = []gatewayv1.Gateway{
		unprogrammedGateway(reasonAddressNotAssigned, "No addresses have been assigned to the Gateway"),
	}

	finding := expectOne(t, evaluate(t, SignalGatewayUnprogrammed, snapshot))
	expectDetail(t, finding, "no LoadBalancer address")
}

// The reader whose tunnel is up has every reason to assume the address does
// not matter, so the rule says otherwise out loud.
func TestGatewayUnprogrammedCorrectsTheCloudflaredAssumption(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.CloudflaredEnabled = true
	snapshot.Gateways = []gatewayv1.Gateway{
		unprogrammedGateway(reasonAddressNotAssigned, "No addresses have been assigned"),
	}

	expectDetail(t, expectOne(t, evaluate(t, SignalGatewayUnprogrammed, snapshot)),
		"cloudflared does not remove that need")
}

// A Gateway unprogrammed for some other reason still fires, but must not
// invent the address diagnosis.
func TestGatewayUnprogrammedDoesNotGuessTheAddressCase(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Gateways = []gatewayv1.Gateway{
		unprogrammedGateway("Invalid", "listener kitchen-https references a Secret that does not exist"),
	}

	finding := expectOne(t, evaluate(t, SignalGatewayUnprogrammed, snapshot))
	expectDetail(t, finding, "references a Secret that does not exist")
	if strings.Contains(finding.Detail, "LoadBalancer address") {
		t.Fatalf("the address diagnosis was applied to an unrelated reason: %s", finding.Detail)
	}
}

func TestGatewayUnprogrammedStaysQuietWhenProgrammed(t *testing.T) {
	snapshot := newSnapshot()
	gateway := unprogrammedGateway(reasonAddressNotAssigned, "")
	gateway.Status.Conditions[0].Status = metav1.ConditionTrue
	snapshot.Gateways = []gatewayv1.Gateway{gateway}
	expectNone(t, evaluate(t, SignalGatewayUnprogrammed, snapshot))
}

func TestRouteRejectedFiresAndAttributesToTheEnvironment(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Routes = []gatewayv1.HTTPRoute{{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testEnvironment,
			Namespace: controller.AppNamespace(testProject),
			Labels: map[string]string{
				controller.LabelProject:     testProject,
				controller.LabelEnvironment: testEnvironment,
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{testHost}},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{
			Parents: []gatewayv1.RouteParentStatus{{
				ControllerName: "io.cilium/gateway-controller",
				Conditions: []metav1.Condition{{
					Type:    string(gatewayv1.RouteConditionAccepted),
					Status:  metav1.ConditionFalse,
					Reason:  "NoMatchingListenerHostname",
					Message: "no listener serves this hostname",
				}},
			}},
		}},
	}}

	finding := expectOne(t, evaluate(t, SignalRouteRejected, snapshot))
	if finding.Fingerprint != "route.rejected/shop/pr-41" {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
	expectDetail(t, finding, "no listener serves this hostname")
}

func TestRouteRejectedStaysQuietOnAnAcceptedRoute(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Routes = []gatewayv1.HTTPRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvironment},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{
			Parents: []gatewayv1.RouteParentStatus{{Conditions: []metav1.Condition{
				{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue},
				{Type: string(gatewayv1.RouteConditionResolvedRefs), Status: metav1.ConditionTrue},
			}}},
		}},
	}}
	expectNone(t, evaluate(t, SignalRouteRejected, snapshot))
}

func TestDNSMismatchFiresOnTheWrongAddress(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.GatewayAddress = testGatewayIP
	snapshot.DNS = []DNSProbe{{Host: testHost, Addresses: []string{"198.51.100.7"}, Exists: true}}

	finding := expectOne(t, evaluate(t, SignalDNSMismatch, snapshot))
	expectDetail(t, finding, "198.51.100.7")
	expectDetail(t, finding, "visitors never reach the edge")
}

func TestDNSMismatchFiresOnAMissingRecord(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.GatewayAddress = testGatewayIP
	snapshot.DNS = []DNSProbe{{Host: testHost}}

	expectDetail(t, expectOne(t, evaluate(t, SignalDNSMismatch, snapshot)), "does not resolve")
}

func TestDNSMismatchStaysQuietWhenTheNamePointsHere(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.GatewayAddress = testGatewayIP
	snapshot.DNS = []DNSProbe{{Host: testHost, Addresses: []string{testGatewayIP}, Exists: true}}
	expectNone(t, evaluate(t, SignalDNSMismatch, snapshot))
}

// The rule's original reading of a Gateway address was "what public DNS should
// name", which is false wherever a router forwards to it: the Gateway is at
// 10.0.10.240, the record says 85.195.238.240, and both are right. Two
// criticals per published hostname is the wrong answer to a correct install.
func TestDNSMismatchStaysQuietBehindNAT(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.GatewayAddress = testPrivateGatewayIP
	snapshot.DNS = []DNSProbe{{Host: testHost, Addresses: []string{testPublicIP}, Exists: true}}
	expectNone(t, evaluate(t, SignalDNSMismatch, snapshot))
}

// Declining to compare is not declining to look. A published name answering
// nothing at all is broken under every topology, NAT included.
func TestDNSMismatchStillFiresOnAMissingRecordBehindNAT(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.GatewayAddress = testPrivateGatewayIP
	snapshot.DNS = []DNSProbe{{Host: testHost}}

	finding := expectOne(t, evaluate(t, SignalDNSMismatch, snapshot))
	expectDetail(t, finding, "no address record at all")
}

// Declaring the public address is what gives the check its teeth back: the
// operator has said what the record should say, so a record saying anything
// else is a finding again.
func TestDNSMismatchFiresAgainstADeclaredPublicAddress(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.GatewayAddress = testPrivateGatewayIP
	snapshot.Platform.PublicAddresses = []string{testPublicIP}
	snapshot.DNS = []DNSProbe{{Host: testHost, Addresses: []string{"192.0.2.99"}, Exists: true}}

	finding := expectOne(t, evaluate(t, SignalDNSMismatch, snapshot))
	expectDetail(t, finding, testPublicIP)
	expectDetail(t, finding, "192.0.2.99")
}

func TestDNSMismatchStaysQuietOnADeclaredPublicAddress(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.GatewayAddress = testPrivateGatewayIP
	snapshot.Platform.PublicAddresses = []string{testPublicIP}
	snapshot.DNS = []DNSProbe{{Host: testHost, Addresses: []string{testPublicIP}, Exists: true}}
	expectNone(t, evaluate(t, SignalDNSMismatch, snapshot))
}

// Split horizon is the usual companion of the translation: the operator
// resolves from inside the cluster, where the answer is the address traffic
// actually lands on. That is the same correct configuration from the other
// side of the router.
func TestDNSMismatchAcceptsTheGatewayAddressUnderSplitHorizon(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.GatewayAddress = testPrivateGatewayIP
	snapshot.Platform.PublicAddresses = []string{testPublicIP}
	snapshot.DNS = []DNSProbe{{Host: testHost, Addresses: []string{testPrivateGatewayIP}, Exists: true}}
	expectNone(t, evaluate(t, SignalDNSMismatch, snapshot))
}

// A Gateway addressed by hostname is the other thing a resolved address can
// never equal. It used to fire on every name; now it is simply not comparable.
func TestDNSMismatchStaysQuietForAGatewayAddressedByHostname(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.GatewayAddress = "lb-1234.example-cloud.net"
	snapshot.DNS = []DNSProbe{{Host: testHost, Addresses: []string{testPublicIP}, Exists: true}}
	expectNone(t, evaluate(t, SignalDNSMismatch, snapshot))
}

func TestPubliclyRoutableRejectsWhatNoPublicRecordShouldName(t *testing.T) {
	for _, address := range []string{
		"10.0.10.240", "172.16.4.1", "192.168.1.1", // RFC1918
		"100.64.0.1",       // carrier-grade NAT
		"127.0.0.1", "::1", // loopback
		"169.254.10.1", "fe80::1", // link-local
		"fd00::1",            // unique local
		"0.0.0.0",            // unspecified
		"lb.example.net", "", // not addresses at all
	} {
		if publiclyRoutable(address) {
			t.Errorf("publiclyRoutable(%q) = true, want false", address)
		}
	}
	for _, address := range []string{"203.0.113.10", "198.51.100.7", "2001:db8::1"} {
		if !publiclyRoutable(address) {
			t.Errorf("publiclyRoutable(%q) = false, want true", address)
		}
	}
}

func certificate(notAfter time.Time, ready bool) Certificate {
	return Certificate{
		Namespace: controller.PlatformNamespace,
		Name:      "kitchen-wildcard",
		DNSNames:  []string{"*.example.com"},
		NotAfter:  notAfter,
		Ready:     ready,
	}
}

func TestCertExpiringFiresWithTheACMEErrorAttached(t *testing.T) {
	snapshot := newSnapshot()
	stuck := certificate(testNow.Add(5*24*time.Hour), false)
	stuck.Reason = "Failed"
	stuck.Message = "propagation check failed: NS ns1.example.com returned NXDOMAIN"
	snapshot.Certificates = []Certificate{stuck}

	finding := expectOne(t, evaluate(t, SignalCertExpiring, snapshot))
	expectDetail(t, finding, "NXDOMAIN")
	expectDetail(t, finding, "DNS-01 only")
}

// A ready certificate inside the window is one cert-manager is about to renew,
// which is not news.
func TestCertExpiringStaysQuietWhileRenewalProgresses(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Certificates = []Certificate{certificate(testNow.Add(10*24*time.Hour), true)}
	expectNone(t, evaluate(t, SignalCertExpiring, snapshot))
}

// A stuck renewal reports itself on Issuing while Ready stays true on the
// still-valid old certificate, so that is the only place it says so.
func TestCertExpiringFiresOnAStuckRenewal(t *testing.T) {
	snapshot := newSnapshot()
	renewing := certificate(testNow.Add(10*24*time.Hour), true)
	renewing.IssuingMessage = "Failed: too many certificates already issued for example.com"
	snapshot.Certificates = []Certificate{renewing}

	expectDetail(t, expectOne(t, evaluate(t, SignalCertExpiring, snapshot)),
		"too many certificates already issued")
}

func TestCertExpiringIgnoresAFreshCertificate(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Certificates = []Certificate{certificate(testNow.Add(80*24*time.Hour), true)}
	expectNone(t, evaluate(t, SignalCertExpiring, snapshot))
}

func cloudflared(available int32) appsv1.Deployment {
	replicas := int32(1)
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cloudflaredDeploymentName,
			Namespace: controller.PlatformNamespace,
		},
		Spec:   appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{AvailableReplicas: available},
	}
}

func TestTunnelDownFires(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Deployments = []appsv1.Deployment{cloudflared(0)}

	finding := expectOne(t, evaluate(t, SignalTunnelDown, snapshot))
	if finding.Title != "the tunnel is down" {
		t.Fatalf("title = %q", finding.Title)
	}
	expectDetail(t, finding, "the only way in")
}

func TestTunnelFlappingFiresWhileAvailable(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Deployments = []appsv1.Deployment{cloudflared(1)}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "kitchen-cloudflared-abc",
		Namespace: controller.PlatformNamespace,
		Labels:    map[string]string{labelComponentKind: cloudflaredComponent},
	}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: cloudflaredComponent, RestartCount: 7,
	}}
	snapshot.Pods = []corev1.Pod{pod}

	finding := expectOne(t, evaluate(t, SignalTunnelDown, snapshot))
	if finding.Title != "the tunnel is flapping" {
		t.Fatalf("title = %q", finding.Title)
	}
}

// No tunnel configured is not a tunnel that is down.
func TestTunnelDownStaysQuietWithoutADeployment(t *testing.T) {
	expectNone(t, evaluate(t, SignalTunnelDown, newSnapshot()))
}

func TestUnroutedHostsFiresOnSustainedTraffic(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.UnroutedHosts = []clickhouse.UnroutedHost{{
		Host:      "old.example.com",
		Requests:  4000,
		FirstSeen: testNow.Add(-50 * time.Minute),
		LastSeen:  testNow.Add(-time.Minute),
	}}

	finding := expectOne(t, evaluate(t, SignalUnroutedHosts, snapshot))
	expectDetail(t, finding, "stale DNS record")
	if finding.Fingerprint != "edge.unrouted-hosts/old.example.com" {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
}

// The platform's own surfaces are published by routes that carry no project,
// so their traffic lands in the store's unattributed bucket beside the stale
// records. The rule subtracts the routes before it accuses anybody.
func TestUnroutedHostsIgnoresThePlatformsOwnHosts(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Routes = []gatewayv1.HTTPRoute{platformRoute("kitchen.example.com")}
	snapshot.UnroutedHosts = []clickhouse.UnroutedHost{{
		Host:      "kitchen.example.com",
		Requests:  4000,
		FirstSeen: testNow.Add(-50 * time.Minute),
		LastSeen:  testNow.Add(-time.Minute),
	}}
	expectNone(t, evaluate(t, SignalUnroutedHosts, snapshot))
}

// A host the platform did publish, asked for with the port and the trailing
// dot a client is free to send, is still that host.
func TestUnroutedHostsMatchesRoutesLoosely(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Routes = []gatewayv1.HTTPRoute{
		platformRoute("Kitchen.example.com"),
		platformRoute("*.apps.example.com"),
	}
	snapshot.UnroutedHosts = []clickhouse.UnroutedHost{{
		Host:      "kitchen.example.com.:443",
		Requests:  4000,
		FirstSeen: testNow.Add(-50 * time.Minute),
		LastSeen:  testNow.Add(-time.Minute),
	}, {
		Host:      "shop.apps.example.com",
		Requests:  4000,
		FirstSeen: testNow.Add(-50 * time.Minute),
		LastSeen:  testNow.Add(-time.Minute),
	}}
	expectNone(t, evaluate(t, SignalUnroutedHosts, snapshot))
}

// The HTTPS redirect the Kitchen reconciler writes on port 80 names no
// hostname of its own. Reading that as "every host is published" would silence
// the rule on every acme installation.
func TestUnroutedHostsStillFiresBesideAHostnamelessRoute(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Routes = []gatewayv1.HTTPRoute{{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kitchen-https-redirect",
			Namespace: controller.PlatformNamespace,
		},
	}}
	snapshot.UnroutedHosts = []clickhouse.UnroutedHost{{
		Host:      "old.example.com",
		Requests:  4000,
		FirstSeen: testNow.Add(-50 * time.Minute),
		LastSeen:  testNow.Add(-time.Minute),
	}}
	expectOne(t, evaluate(t, SignalUnroutedHosts, snapshot))
}

// A route listing that failed must make the rule say so rather than let it
// accuse the platform's own hostname.
func TestUnroutedHostsIsUnknownWithoutTheRoutes(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.UnroutedHosts = []clickhouse.UnroutedHost{{
		Host:      "kitchen.example.com",
		Requests:  4000,
		FirstSeen: testNow.Add(-50 * time.Minute),
		LastSeen:  testNow.Add(-time.Minute),
	}}
	snapshot.MarkUnreadable(InputRoutes, "the api server said no")

	finding := expectOne(t, evaluate(t, SignalUnroutedHosts, snapshot))
	if finding.Severity != SeverityUnknown {
		t.Fatalf("severity = %q", finding.Severity)
	}
}

// platformRoute is one of the platform's own surfaces: published on the shared
// Gateway, labelled with no project, because its traffic belongs to none.
func platformRoute(host string) gatewayv1.HTTPRoute {
	return gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kitchen-api",
			Namespace: controller.PlatformNamespace,
		},
		Spec: gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(host)}},
	}
}

// A scanner asks once and goes away.
func TestUnroutedHostsIgnoresABurst(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.UnroutedHosts = []clickhouse.UnroutedHost{{
		Host:      "scanner.example.net",
		Requests:  400,
		FirstSeen: testNow.Add(-2 * time.Minute),
		LastSeen:  testNow.Add(-time.Minute),
	}}
	expectNone(t, evaluate(t, SignalUnroutedHosts, snapshot))
}
