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

package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/flows"
	"github.com/Bermos/Kitchen/internal/signals"
)

const (
	platformSignalsPath    = "/api/v1/platform/signals"
	environmentSignalsPath = "/api/v1/environments/" + testEnvironment + "/signals"
)

// crashLoopingPod is the condition the catalogue's first rule exists for, in
// the shape the API server reports it.
func crashLoopingPod(name string) *corev1.Pod {
	pod := runningPod(name)
	pod.Labels[controller.LabelProject] = feedProject
	pod.Status.ContainerStatuses[0].Ready = false
	pod.Status.ContainerStatuses[0].RestartCount = 14
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{
			Reason:  "CrashLoopBackOff",
			Message: "back-off 5m0s restarting failed container",
		},
	}
	return pod
}

// The environment's strip is the catalogue narrowed to one environment: its own
// findings, and its project's.
func TestEnvironmentSignalsAreTheEnvironmentsOwn(t *testing.T) {
	other := crashLoopingPod("blog-production-1")
	other.Labels[controller.LabelProject] = otherProject
	other.Labels[controller.LabelEnvironment] = otherProject + "-production"
	other.Namespace = controller.AppNamespace(otherProject)

	h := newHarness(t, nil, append(fixtures(),
		crashLoopingPod("shop-production-7d9f4"), other)...)

	res := h.do(t, http.MethodGet, environmentSignalsPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", environmentSignalsPath, res.Code, res.Body.String())
	}
	body := decode[signalsBody](t, res)
	if body.Project != feedProject || body.Environment != testEnvironment {
		t.Errorf("the answer should name what it is about: %+v", body)
	}
	if len(body.Items) == 0 {
		t.Fatalf("a crash-looping container is a finding: %+v", body)
	}
	for _, finding := range body.Items {
		if finding.Scope.Project != feedProject {
			t.Errorf("another project's finding reached this strip: %+v", finding)
		}
		if finding.Evidence == "" || finding.Fingerprint == "" {
			t.Errorf("every finding carries its evidence and its identity: %+v", finding)
		}
	}
	if body.Counts.Critical+body.Counts.Warning == 0 {
		t.Errorf("the strip counts what it shows: %+v", body.Counts)
	}
	// The store reads are narrowed to this one environment, which is what makes
	// a strip on every page load affordable.
	if h.logs.lastRequestSeries.Environment != testEnvironment {
		t.Errorf("the per-environment reads should be narrowed: %+v", h.logs.lastRequestSeries)
	}
}

// The problems list is every firing finding, worst first.
func TestPlatformSignalsListsWhatIsFiring(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), crashLoopingPod("shop-production-7d9f4"))...)

	res := h.do(t, http.MethodGet, platformSignalsPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", platformSignalsPath, res.Code, res.Body.String())
	}
	body := decode[signalsBody](t, res)
	if len(body.Items) == 0 {
		t.Fatalf("the crash loop should be on the platform's list too: %+v", body)
	}
	worst := body.Items[0]
	for _, finding := range body.Items {
		if finding.Severity.Rank() > worst.Severity.Rank() {
			t.Fatalf("the list is not ordered worst first: %+v", body.Items)
		}
		if finding.Severity == signals.SeverityUnknown {
			t.Errorf("a rule that could not be evaluated is not a problem: %+v", finding)
		}
		worst = finding
	}
	if body.EvaluatedAt.IsZero() {
		t.Error("findings are ephemeral, so the answer has to say when it was taken")
	}
}

// A store that is configured and unreachable is the case this endpoint must not
// get wrong: the rules over it cannot see, and reporting an empty problems list
// would be the platform claiming health it never measured.
func TestPlatformSignalsSayWhenTheStoreCannotBeRead(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.server.logStore = func(context.Context) (logReader, error) {
		return nil, errors.New("dial tcp 10.0.0.1:8123: connect: connection refused")
	}

	res := h.do(t, http.MethodGet, platformSignalsPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", platformSignalsPath, res.Code, res.Body.String())
	}
	body := decode[signalsBody](t, res)
	if len(body.Unreadable) == 0 {
		t.Fatalf("an unreachable store is not an absence of problems: %+v", body)
	}
	inputs := map[string]bool{}
	for _, failure := range body.Unreadable {
		inputs[string(failure.Input)] = true
		if failure.Reason == "" {
			t.Errorf("an unreadable input has to say why: %+v", failure)
		}
	}
	// Every store-backed input, named once each rather than once per rule.
	for _, input := range []signals.Input{
		signals.InputRequests, signals.InputResources, signals.InputClusterEvents,
		signals.InputFreshness, signals.InputStore,
	} {
		if !inputs[string(input)] {
			t.Errorf("%s could not be read and should say so: %+v", input, body.Unreadable)
		}
	}
}

// An installation that deliberately runs without telemetry is a different
// answer: those rules do not arise, and a permanent row saying so would train
// the reader to ignore the list.
func TestPlatformSignalsWithoutATelemetryStore(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.server.logStore = func(context.Context) (logReader, error) { return nil, errNoLogStore }

	res := h.do(t, http.MethodGet, platformSignalsPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", platformSignalsPath, res.Code, res.Body.String())
	}
	body := decode[signalsBody](t, res)
	if len(body.Unreadable) != 0 {
		t.Errorf("no store is a configuration, not a failure: %+v", body.Unreadable)
	}
}

// The gatherer's sources are the wiring this file is really about: the follower
// adapted to the ingest accounting, a resolver that cannot make broken DNS out
// of its own failure, and two sources deliberately left unset.
func TestSignalSourcesWiring(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.server.Flows = stubFollower{loss: flows.Loss{
		Events: 900, Reconnects: 2, Window: flows.LossWindow, Latest: time.Now(),
	}}

	sources := h.server.signalSources(context.Background())
	if sources.Ingest == nil {
		t.Fatal("the follower is the only thing that sees Relay's losses, so it has to reach the gatherer")
	}
	health, err := sources.Ingest.IngestHealth(context.Background())
	if err != nil {
		t.Fatalf("reading the ledger cannot fail: %v", err)
	}
	if health.FlowsLost != 900 || health.Reconnects != 2 || health.Window != flows.LossWindow {
		t.Errorf("the ledger should arrive intact: %+v", health)
	}
	// Both come off the store this harness has, so the three rules over them
	// are lit rather than dark.
	if sources.HostMetrics == nil || sources.VolumeUsage == nil {
		t.Errorf("a readable store lights both: %+v / %+v", sources.HostMetrics, sources.VolumeUsage)
	}
	if sources.Resolver == nil {
		t.Error("the DNS rule needs a resolver")
	}
	if sources.Client == nil || sources.Store == nil {
		t.Error("the cluster and the store are both readable in this harness")
	}
}

// The two optional sources follow the store's own resolution, and the two ways
// it can be missing are not the same answer. Getting this backwards is how a
// screen ends up saying a disk is fine because nothing ever measured it.
func TestOptionalSourcesFollowTheStore(t *testing.T) {
	unreachable := errors.New("dial tcp 10.0.0.1:8123: connect: connection refused")
	for _, tc := range []struct {
		name  string
		err   error
		wired bool
	}{
		{"no store at all leaves them unset", errNoLogStore, false},
		{"an unreachable store still wires them", unreachable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil, fixtures()...)
			h.server.logStore = func(context.Context) (logReader, error) { return nil, tc.err }

			sources := h.server.signalSources(context.Background())
			if wired := sources.HostMetrics != nil; wired != tc.wired {
				t.Errorf("host metrics wired = %v, want %v", wired, tc.wired)
			}
			if wired := sources.VolumeUsage != nil; wired != tc.wired {
				t.Errorf("volume usage wired = %v, want %v", wired, tc.wired)
			}
			if !tc.wired {
				return
			}
			// Wired, and every read fails with the reason — which is what makes
			// the gatherer call the input unreadable rather than absent.
			if _, err := sources.VolumeUsage.VolumeUsage(context.Background(), time.Now()); !errors.Is(err, unreachable) {
				t.Errorf("an unreachable store has to fail the read, got %v", err)
			}
		})
	}
}

// A resolver that is itself broken must not read as broken DNS. The gatherer
// tells the two apart by the answer's own shape, so this checks the shape the
// API hands it.
func TestABrokenResolverIsNotBrokenDNS(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	giveTheGatewayAnAddress(t, h)
	h.server.resolver = failingResolver{err: &net.DNSError{
		Err: "server misbehaving", Name: "shop.apps.example.com", IsTimeout: true,
	}}

	res := h.do(t, http.MethodGet, platformSignalsPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", platformSignalsPath, res.Code, res.Body.String())
	}
	body := decode[signalsBody](t, res)

	var dnsUnreadable bool
	for _, failure := range body.Unreadable {
		if failure.Input == signals.InputDNS {
			dnsUnreadable = true
		}
	}
	if !dnsUnreadable {
		t.Fatalf("a resolver that misbehaved is an input that could not be read: %+v", body.Unreadable)
	}
	for _, finding := range body.Items {
		if strings.HasPrefix(string(finding.Signal), "dns.") {
			t.Errorf("nothing was learned about DNS, so nothing may be reported about it: %+v", finding)
		}
	}
}

// A name that genuinely does not resolve is the finding the rule exists for,
// which is the other half of the same distinction.
func TestAMissingRecordIsAFinding(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	giveTheGatewayAnAddress(t, h)
	h.server.resolver = failingResolver{err: &net.DNSError{
		Err: "no such host", Name: "shop.apps.example.com", IsNotFound: true,
	}}

	res := h.do(t, http.MethodGet, platformSignalsPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", platformSignalsPath, res.Code, res.Body.String())
	}
	body := decode[signalsBody](t, res)
	for _, failure := range body.Unreadable {
		if failure.Input == signals.InputDNS {
			t.Fatalf("the resolver answered; nothing was unreadable: %+v", failure)
		}
	}
	var found bool
	for _, finding := range body.Items {
		if strings.HasPrefix(string(finding.Signal), "dns.") {
			found = true
		}
	}
	if !found {
		t.Errorf("a published name that does not exist is a problem: %+v", body.Items)
	}
}

// giveTheGatewayAnAddress is what the DNS rule needs before it probes anything:
// an address for a published name to be compared against, and no tunnel in
// front of it. Behind cloudflared the names point at Cloudflare by design, and
// the rule correctly says nothing.
func giveTheGatewayAnAddress(t *testing.T, h *harness) {
	t.Helper()
	kitchen := &kitchenv1alpha1.Kitchen{}
	key := types.NamespacedName{Name: controller.KitchenSingletonName}
	if err := h.server.Client.Get(context.Background(), key, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Status.GatewayAddress = "203.0.113.10"
	if err := h.server.Client.Update(context.Background(), kitchen); err != nil {
		t.Fatal(err)
	}
}

// failingResolver answers every lookup the same way, which is how the two
// failures that must not be confused are exercised.
type failingResolver struct{ err error }

func (r failingResolver) LookupHost(context.Context, string) ([]string, error) { return nil, r.err }
