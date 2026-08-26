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
	"context"
	"errors"
	"net"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// storeDown is the error a gather against an unreachable store reports, and
// the string the degradation assertions look for.
var storeDown = errors.New("dial tcp 10.0.0.5:8123: connect: connection refused")

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		kitchenv1alpha1.AddToScheme,
		gatewayv1.Install,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("building the scheme: %v", err)
		}
	}
	return scheme
}

func testClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
}

// kitchenSingleton is the platform object every gather reads first.
func kitchenSingleton(cloudflared bool) *kitchenv1alpha1.Kitchen {
	return &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: controller.KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			BaseDomain: "example.com",
			Ingress: kitchenv1alpha1.IngressSpec{
				Cloudflared: kitchenv1alpha1.CloudflaredSpec{Enabled: cloudflared},
			},
			Observability: kitchenv1alpha1.ObservabilitySpec{
				ClickHouse: kitchenv1alpha1.ClickHouseSpec{RetentionDays: 30},
			},
		},
		Status: kitchenv1alpha1.KitchenStatus{GatewayAddress: testGatewayIP},
	}
}

// testStoreBytes is what the stubbed store reports occupying, and the number
// the capacity assertions are read against.
const testStoreBytes = 10 << 30

// stubStore answers every read with what it was given, or fails every read.
type stubStore struct {
	err       error
	freshness []clickhouse.NodeFreshness
	events    []clickhouse.K8sEvent
	// storeStatsReads counts the store-health reads one gather makes, which is
	// how the "one narrow read, not the dashboard's overview" contract is
	// asserted rather than assumed.
	storeStatsReads int
	// unroutedErr fails the unrouted-host read alone. It is its own field
	// because that read is the only one in a gather that goes to the raw
	// request rows rather than to the minute rollup over them.
	unroutedErr error
}

func (s *stubStore) RequestSeries(context.Context, clickhouse.RequestSeriesQuery) (clickhouse.RequestSeries, error) {
	return clickhouse.RequestSeries{}, s.err
}

func (s *stubStore) ResourceSeries(context.Context, clickhouse.ResourceSeriesQuery) (clickhouse.ResourceSeries, error) {
	return clickhouse.ResourceSeries{}, s.err
}

func (s *stubStore) ProjectTraffic(context.Context, clickhouse.ProjectTrafficQuery) ([]clickhouse.ProjectTraffic, error) {
	return nil, s.err
}

func (s *stubStore) UnroutedHosts(context.Context, clickhouse.PlatformRequestsQuery) ([]clickhouse.UnroutedHost, error) {
	if s.unroutedErr != nil {
		return nil, s.unroutedErr
	}
	return nil, s.err
}

func (s *stubStore) QueryK8sEvents(context.Context, clickhouse.K8sEventQuery) ([]clickhouse.K8sEvent, error) {
	return s.events, s.err
}

func (s *stubStore) TelemetryFreshness(context.Context, time.Duration) ([]clickhouse.NodeFreshness, error) {
	return s.freshness, s.err
}

func (s *stubStore) StoreStats(context.Context) (clickhouse.StoreStats, error) {
	s.storeStatsReads++
	return clickhouse.StoreStats{BytesOnDisk: testStoreBytes}, s.err
}

// stubResolver answers a fixed map, and anything not in it either does not
// exist or makes the resolver itself misbehave.
type stubResolver struct {
	answers map[string][]string
	err     error
}

func (r *stubResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	if addresses, ok := r.answers[host]; ok {
		return addresses, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func TestGatherReadsTheCluster(t *testing.T) {
	environment := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvironment, Namespace: controller.PlatformNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: testProject},
		},
	}
	pod := readyPod()
	worker := node(testNode, "4", "16Gi")

	snapshot := Gather(context.Background(), Sources{
		Client: testClient(t, kitchenSingleton(false), environment, &pod, &worker),
		Now:    func() time.Time { return testNow },
	}, Options{})

	if len(snapshot.Pods) != 1 || len(snapshot.Nodes) != 1 || len(snapshot.Environments) != 1 {
		t.Fatalf("gathered %d pods, %d nodes, %d environments",
			len(snapshot.Pods), len(snapshot.Nodes), len(snapshot.Environments))
	}
	if snapshot.Platform.BaseDomain != "example.com" || snapshot.Platform.RetentionDays != 30 {
		t.Fatalf("platform facts = %+v", snapshot.Platform)
	}
	if !snapshot.Now.Equal(testNow) {
		t.Fatalf("now = %s, want the injected clock", snapshot.Now)
	}
}

// No API client at all is a broken gather, and every rule that reads the
// cluster must say so rather than report a healthy empty cluster.
func TestGatherWithoutAClientMarksEveryClusterInput(t *testing.T) {
	snapshot := Gather(context.Background(), Sources{Now: func() time.Time { return testNow }}, Options{})
	for _, input := range []Input{InputPods, InputNodes, InputWorkloads, InputKitchen} {
		if snapshot.Available(input) {
			t.Errorf("%s was reported available with no client", input)
		}
	}
}

// The degradation contract, end to end: a store that is down produces rules
// that say they could not run, never rules that say everything is fine.
func TestGatherWithAnUnreachableStoreDegradesHonestly(t *testing.T) {
	snapshot := Gather(context.Background(), Sources{
		Client: testClient(t, kitchenSingleton(false)),
		Store:  &stubStore{err: storeDown},
		Now:    func() time.Time { return testNow },
	}, Options{})

	for _, input := range []Input{
		InputRawRequests, InputRequests, InputClusterEvents, InputFreshness, InputStore,
	} {
		if snapshot.Available(input) {
			t.Errorf("%s was reported available against a store that refused every read", input)
		}
	}
	failures := snapshot.Unreadable()
	if len(failures) == 0 {
		t.Fatal("nothing was reported unreadable")
	}

	findings := Catalogue().Evaluate(snapshot)
	unknown := 0
	for _, finding := range findings {
		if finding.Severity == SeverityUnknown {
			unknown++
		}
	}
	if unknown == 0 {
		t.Fatalf("no rule reported that it could not be evaluated: %s", describe(findings))
	}
}

// The unrouted bucket is the one read in a gather that groups over the raw
// http_requests rows; every other traffic read is over the minute rollup. So a
// failure there marks the raw table and nothing else — naming the rollup would
// tell the operator a table the failing query never touched could not be read,
// and would take the six rules that do read the rollup dark along with it.
func TestGatherMarksTheRawRequestTableForTheUnroutedRead(t *testing.T) {
	snapshot := Gather(context.Background(), Sources{
		Client: testClient(t, kitchenSingleton(false)),
		Store:  &stubStore{unroutedErr: storeDown},
		Now:    func() time.Time { return testNow },
	}, Options{})

	if snapshot.Available(InputRawRequests) {
		t.Fatal("the query that failed was over the raw request rows, and nothing says so")
	}
	if !snapshot.Available(InputRequests) {
		t.Fatal("the minute rollup answered, and must not be marked for another table's failure")
	}
	failures := snapshot.Unreadable()
	if len(failures) != 1 || failures[0].Input != InputRawRequests {
		t.Fatalf("want the raw table named once and nothing else: %+v", failures)
	}
	if failures[0].Reason == "" {
		t.Errorf("an unreadable input has to say why: %+v", failures[0])
	}

	// The rule over the raw table says it could not be evaluated; the rules over
	// the rollup carry on as normal.
	if finding := expectOne(t, evaluate(t, SignalUnroutedHosts, snapshot)); finding.Severity != SeverityUnknown {
		t.Fatalf("severity = %q, want unknown", finding.Severity)
	}
	for _, id := range []ID{
		SignalErrorRate, SignalLatencyRegressed, SignalTrafficVanished, SignalNoBackend,
		SignalLatencyCorrelated, SignalErrorCorrelated,
	} {
		for _, finding := range evaluate(t, id, snapshot) {
			if finding.Severity == SeverityUnknown {
				t.Errorf("%s went dark for a failure in a table it does not read: %s", id, finding.Detail)
			}
		}
	}
}

// No store configured is not a broken store: an installation without
// observability wired up has nothing to report, and a permanent list of rules
// that "could not be evaluated" would be worse than saying nothing.
func TestGatherWithNoStoreConfiguredIsNotAFault(t *testing.T) {
	snapshot := Gather(context.Background(), Sources{
		Client: testClient(t, kitchenSingleton(false)),
		Now:    func() time.Time { return testNow },
	}, Options{})

	if len(snapshot.Unreadable()) != 0 {
		t.Fatalf("an unconfigured store was reported as unreadable: %+v", snapshot.Unreadable())
	}
	for _, finding := range Catalogue().Evaluate(snapshot) {
		if finding.Severity == SeverityUnknown {
			t.Fatalf("%s reported it could not be evaluated: %s", finding.Signal, finding.Detail)
		}
	}
}

// The freshness query's contract: a node absent from the answer is silent.
func TestGatherCarriesFreshnessThroughToNodeSilent(t *testing.T) {
	quiet := node(testNode, "4", "16Gi")
	reporting := node(testOtherNode, "4", "16Gi")

	snapshot := Gather(context.Background(), Sources{
		Client: testClient(t, kitchenSingleton(false), &quiet, &reporting),
		Store: &stubStore{freshness: []clickhouse.NodeFreshness{
			{Node: testOtherNode, LastSeen: testNow.Add(-time.Minute)},
		}},
		Now: func() time.Time { return testNow },
	}, Options{})

	findings := evaluate(t, SignalNodeSilent, snapshot)
	finding := expectOne(t, findings)
	if finding.Scope.Node != testNode {
		t.Fatalf("silent node = %q, want the one absent from the answer", finding.Scope.Node)
	}
}

func TestGatherResolvesPublishedNames(t *testing.T) {
	environment := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvironment, Namespace: controller.PlatformNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: testProject},
		},
		Status: kitchenv1alpha1.EnvironmentStatus{URL: "https://" + testHost},
	}

	snapshot := Gather(context.Background(), Sources{
		Client:   testClient(t, kitchenSingleton(false), environment),
		Resolver: &stubResolver{answers: map[string][]string{testHost: {"198.51.100.7"}}},
		Now:      func() time.Time { return testNow },
	}, Options{})

	if len(snapshot.DNS) != 1 || snapshot.DNS[0].Host != testHost {
		t.Fatalf("probes = %+v", snapshot.DNS)
	}
	expectDetail(t, expectOne(t, evaluate(t, SignalDNSMismatch, snapshot)), "198.51.100.7")
}

// A resolver that is itself broken must never look like broken DNS. This is
// the case the rule was written around, so it is the one asserted hardest.
func TestGatherTreatsAFailingResolverAsAnUnreadableInput(t *testing.T) {
	environment := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvironment, Namespace: controller.PlatformNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: testProject},
		},
		Status: kitchenv1alpha1.EnvironmentStatus{URL: "https://" + testHost},
	}
	broken := &stubResolver{err: &net.DNSError{Err: "server misbehaving", IsTemporary: true}}

	snapshot := Gather(context.Background(), Sources{
		Client:   testClient(t, kitchenSingleton(false), environment),
		Resolver: broken,
		Now:      func() time.Time { return testNow },
	}, Options{})

	if snapshot.Available(InputDNS) {
		t.Fatal("a misbehaving resolver was reported as a readable DNS input")
	}
	finding := expectOne(t, evaluate(t, SignalDNSMismatch, snapshot))
	if finding.Severity != SeverityUnknown {
		t.Fatalf("severity = %q, want unknown — a broken resolver is not broken DNS", finding.Severity)
	}
}

// Behind cloudflared, names point at Cloudflare by design. A mismatch is the
// correct configuration, so the question does not arise at all.
func TestGatherSkipsDNSBehindCloudflared(t *testing.T) {
	snapshot := Gather(context.Background(), Sources{
		Client:   testClient(t, kitchenSingleton(true)),
		Resolver: &stubResolver{},
		Now:      func() time.Time { return testNow },
	}, Options{})

	if snapshot.Available(InputDNS) {
		t.Fatal("DNS was probed behind a tunnel")
	}
	expectNone(t, evaluate(t, SignalDNSMismatch, snapshot))
}

// Without a Gateway address there is no expected answer, and every name would
// "mismatch" whatever it resolved to.
func TestGatherSkipsDNSWithoutAGatewayAddress(t *testing.T) {
	kitchen := kitchenSingleton(false)
	kitchen.Status.GatewayAddress = ""

	snapshot := Gather(context.Background(), Sources{
		Client:   testClient(t, kitchen),
		Resolver: &stubResolver{},
		Now:      func() time.Time { return testNow },
	}, Options{})

	if snapshot.Available(InputDNS) {
		t.Fatal("DNS was probed with no address to compare against")
	}
}

// A declared public address is an expected answer of its own: nothing about it
// depends on the Gateway having been programmed yet, and it is what a record
// should say either way.
func TestGatherProbesDNSWithOnlyAPublicAddressDeclared(t *testing.T) {
	kitchen := kitchenSingleton(false)
	kitchen.Status.GatewayAddress = ""
	kitchen.Spec.Ingress.PublicAddresses = []string{testPublicIP}
	environment := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvironment, Namespace: controller.PlatformNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: testProject},
		},
		Status: kitchenv1alpha1.EnvironmentStatus{URL: "https://" + testHost},
	}

	snapshot := Gather(context.Background(), Sources{
		Client:   testClient(t, kitchen, environment),
		Resolver: &stubResolver{answers: map[string][]string{testHost: {"192.0.2.99"}}},
		Now:      func() time.Time { return testNow },
	}, Options{})

	if !snapshot.Available(InputDNS) {
		t.Fatal("DNS was not probed against the declared public address")
	}
	expectDetail(t, expectOne(t, evaluate(t, SignalDNSMismatch, snapshot)), testPublicIP)
}

// unstructuredFails answers every unstructured list with a chosen error, which
// is how a cluster without cert-manager's CRD behaves.
type unstructuredFails struct {
	client.Client
	err error
}

func (c unstructuredFails) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, addressed := list.(*unstructured.UnstructuredList); addressed {
		return c.err
	}
	return c.Client.List(ctx, list, opts...)
}

// cert-manager not being installed is a supported configuration — tls.mode
// none, or a certificate supplied by hand — and not a fault.
func TestGatherTreatsAMissingCertificateKindAsNotApplicable(t *testing.T) {
	noCRD := &meta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: "cert-manager.io", Kind: "Certificate"},
	}
	snapshot := Gather(context.Background(), Sources{
		Client: unstructuredFails{Client: testClient(t, kitchenSingleton(false)), err: noCRD},
		Now:    func() time.Time { return testNow },
	}, Options{})

	if snapshot.Available(InputCertificates) {
		t.Fatal("certificates were reported readable without the CRD")
	}
	if len(snapshot.Unreadable()) != 0 {
		t.Fatalf("a missing CRD was reported as unreadable: %+v", snapshot.Unreadable())
	}
	expectNone(t, evaluate(t, SignalCertExpiring, snapshot))
}

// A cluster that has the CRD and refuses the read is a different thing, and
// the rule over it must say it could not be evaluated.
func TestGatherReportsARefusedCertificateRead(t *testing.T) {
	snapshot := Gather(context.Background(), Sources{
		Client: unstructuredFails{
			Client: testClient(t, kitchenSingleton(false)),
			err:    errors.New("certificates.cert-manager.io is forbidden"),
		},
		Now: func() time.Time { return testNow },
	}, Options{})

	finding := expectOne(t, evaluate(t, SignalCertExpiring, snapshot))
	if finding.Severity != SeverityUnknown {
		t.Fatalf("severity = %q, want unknown", finding.Severity)
	}
	expectDetail(t, finding, "is forbidden")
}

// The three sources §7 names that nothing satisfies yet are dark rather than
// broken, and the rules over them must stay silent instead of claiming health.
func TestGatherMarksTheUnwiredSourcesNotApplicable(t *testing.T) {
	snapshot := Gather(context.Background(), Sources{
		Client: testClient(t, kitchenSingleton(false)),
		Store:  &stubStore{},
		Now:    func() time.Time { return testNow },
	}, Options{})

	for _, input := range []Input{InputHostMetrics, InputVolumeStats, InputIngest} {
		state, reason := snapshot.inputState(input)
		if state != inputNotApplicable {
			t.Errorf("%s is in state %v, want not-applicable", input, state)
		}
		if reason == "" {
			t.Errorf("%s was marked without a reason", input)
		}
	}
	for _, id := range []ID{SignalNodeSaturated, SignalNodeDiskFilling, SignalPVCFilling, SignalFlowsLost} {
		expectNone(t, evaluate(t, id, snapshot))
	}
}

// The store's own capacity comes from the API server, because ClickHouse knows
// how much it has written and nothing about the disk underneath.
func TestGatherReadsTheStoresCapacityFromItsClaim(t *testing.T) {
	bound := claim(controller.PlatformNamespace, "data-kitchen-clickhouse-0", corev1.ClaimBound)
	bound.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("50Gi")}

	store := &stubStore{}
	snapshot := Gather(context.Background(), Sources{
		Client: testClient(t, kitchenSingleton(false), &bound),
		Store:  store,
		Now:    func() time.Time { return testNow },
	}, Options{})

	if snapshot.Store.CapacityBytes != 50<<30 {
		t.Fatalf("capacity = %d, want 50Gi from the claim", snapshot.Store.CapacityBytes)
	}
	if snapshot.Store.BytesOnDisk != testStoreBytes {
		t.Fatalf("bytes on disk = %d, want what the store reported", snapshot.Store.BytesOnDisk)
	}
	// The store's health is two numbers, and it is read once per gather through
	// the read that answers only those two. It used to come off the dashboard's
	// overview, which also aggregated a day of flows, logs and events on every
	// platform screen and every diagnostics strip, and threw all of it away.
	if store.storeStatsReads != 1 {
		t.Errorf("the store's health was read %d times, want exactly one narrow read",
			store.storeStatsReads)
	}
}

// A narrowed gather is what the environment page asks for: one environment's
// series, and the whole cluster's objects, because a saturated node is the
// most useful thing to tell a developer whose environment is misbehaving.
func TestGatherNarrowedToOneEnvironmentStillReadsTheCluster(t *testing.T) {
	first := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvironment, Namespace: controller.PlatformNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: testProject},
		},
	}
	second := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: controller.PlatformNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: testProject},
		},
	}
	worker := node(testNode, "4", "16Gi")

	snapshot := Gather(context.Background(), Sources{
		Client: testClient(t, kitchenSingleton(false), first, second, &worker),
		Store:  &stubStore{},
		Now:    func() time.Time { return testNow },
	}, Options{Project: testProject, Environment: testEnvironment})

	if len(snapshot.Traffic) != 1 {
		t.Fatalf("read series for %d environments, want the one asked for", len(snapshot.Traffic))
	}
	if len(snapshot.Environments) != 2 || len(snapshot.Nodes) != 1 {
		t.Fatalf("the cluster reads were narrowed too: %d environments, %d nodes",
			len(snapshot.Environments), len(snapshot.Nodes))
	}
}
