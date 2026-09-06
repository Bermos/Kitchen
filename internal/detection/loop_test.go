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

package detection

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/signals"
)

// What a round has to get right, and what it must not do when it cannot.
//
// The catalogue's own rules are tested in internal/signals against hand-built
// snapshots, and the diffing beside them. What is left here is the loop: that
// it records what changed rather than what it saw, that it publishes what it
// did, and that a store it cannot write to costs the history nothing.

const (
	testProject     = "shop"
	testEnvironment = "shop-production"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		scheme.AddToScheme, kitchenv1alpha1.AddToScheme, gatewayv1.Install,
		appsv1.AddToScheme, batchv1.AddToScheme, corev1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("building the scheme: %v", err)
		}
	}
	return s
}

// singleton is the Kitchen object a round reads itself out of.
func singleton() *kitchenv1alpha1.Kitchen {
	return &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: controller.KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			BaseDomain: "example.test",
		},
	}
}

// crashLoopingPod is the condition the catalogue's first rule exists for, in
// the shape the API server reports it — and the only fixture these tests need,
// because what is under test is the loop rather than the rule.
func crashLoopingPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testProject + "-production-7d9f4",
			Namespace: controller.AppNamespace(testProject),
			Labels: map[string]string{
				controller.LabelProject:     testProject,
				controller.LabelEnvironment: testEnvironment,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         controller.AppContainerName,
				RestartCount: 14,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "CrashLoopBackOff",
					Message: "back-off 5m0s restarting failed container",
				}},
			}},
		},
	}
}

// fakeStore answers every read with nothing and remembers every write. Nothing
// it returns is interesting on purpose: the conditions these tests turn on come
// from the cluster fixtures, and a store that answers emptily is the shape of
// an installation whose telemetry is healthy and quiet.
type fakeStore struct {
	open      []clickhouse.SignalTransition
	written   [][]clickhouse.SignalTransition
	openErr   error
	insertErr error
}

func (f *fakeStore) RequestSeries(
	context.Context, clickhouse.RequestSeriesQuery,
) (clickhouse.RequestSeries, error) {
	return clickhouse.RequestSeries{}, nil
}

func (f *fakeStore) ResourceSeries(
	context.Context, clickhouse.ResourceSeriesQuery,
) (clickhouse.ResourceSeries, error) {
	return clickhouse.ResourceSeries{}, nil
}

func (f *fakeStore) ProjectTraffic(
	context.Context, clickhouse.ProjectTrafficQuery,
) ([]clickhouse.ProjectTraffic, error) {
	return nil, nil
}

func (f *fakeStore) UnroutedHosts(
	context.Context, clickhouse.PlatformRequestsQuery,
) ([]clickhouse.UnroutedHost, error) {
	return nil, nil
}

func (f *fakeStore) QueryK8sEvents(context.Context, clickhouse.K8sEventQuery) ([]clickhouse.K8sEvent, error) {
	return nil, nil
}

func (f *fakeStore) TelemetryFreshness(context.Context, time.Duration) ([]clickhouse.NodeFreshness, error) {
	return nil, nil
}

func (f *fakeStore) StoreStats(context.Context) (clickhouse.StoreStats, error) {
	return clickhouse.StoreStats{}, nil
}

func (f *fakeStore) NodeUsage(context.Context, clickhouse.NodeUsageQuery) ([]clickhouse.NodeUsage, error) {
	return nil, nil
}

func (f *fakeStore) VolumeUsage(context.Context, clickhouse.VolumeUsageQuery) ([]clickhouse.VolumeUsage, error) {
	return nil, nil
}

func (f *fakeStore) OpenSignalTransitions(context.Context) ([]clickhouse.SignalTransition, error) {
	return f.open, f.openErr
}

func (f *fakeStore) InsertSignalTransitions(
	_ context.Context, transitions []clickhouse.SignalTransition,
) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	if len(transitions) > 0 {
		f.written = append(f.written, transitions)
	}
	return nil
}

// nowhere is a resolver that answers nothing, so no test here touches DNS.
type nowhere struct{}

func (nowhere) LookupHost(context.Context, string) ([]string, error) { return nil, nil }

// loopOver builds a loop over a fake cluster and a fake store.
func loopOver(t *testing.T, store Store, objects ...runtime.Object) *Loop {
	t.Helper()
	client := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRuntimeObjects(append([]runtime.Object{singleton()}, objects...)...).
		WithStatusSubresource(&kitchenv1alpha1.Kitchen{}).
		Build()
	return &Loop{
		Client:   client,
		Resolver: nowhere{},
		store:    func(context.Context) (Store, error) { return store, nil },
	}
}

// The loop's whole output: a condition that was not there opens, and a
// condition that has gone resolves. Nothing is written for the rounds in
// between, which is the difference between a history and a sampling.
func TestARoundRecordsWhatChanged(t *testing.T) {
	store := &fakeStore{}
	loop := loopOver(t, store, crashLoopingPod())
	ctx := context.Background()

	round, err := loop.RoundOnce(ctx)
	if err != nil {
		t.Fatalf("the first round: %v", err)
	}
	if !round.Evaluated {
		t.Fatalf("a store and a catalogue is a round: %+v", round)
	}
	if round.Recorded == 0 {
		t.Fatalf("a crash-looping container is a condition that opened: %+v", round)
	}
	for _, transition := range round.Transitions {
		if transition.State != signals.StateOpen {
			t.Errorf("the first round opens: %+v", transition)
		}
	}

	// The same cluster a moment later is not news.
	again, err := loop.RoundOnce(ctx)
	if err != nil {
		t.Fatalf("the second round: %v", err)
	}
	if again.Recorded != 0 {
		t.Fatalf("nothing changed, so nothing is recorded: %+v", again.Transitions)
	}
	if again.Open != round.Open {
		t.Errorf("the same conditions are open: %d then %d", round.Open, again.Open)
	}

	// And what the round did is on the singleton, which is how an operator
	// knows anything is watching at all.
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := loop.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatalf("reading the singleton back: %v", err)
	}
	status := kitchen.Status.Signals
	if status == nil || status.LastEvaluated == nil {
		t.Fatalf("a round that ran says when: %+v", status)
	}
	if status.Open != int32(again.Open) || status.IntervalSeconds == 0 {
		t.Errorf("the status is the round: %+v", status)
	}
}

// A developer condition is delivered twice — to the project and to the
// operator — and the two are separate rows, because they are acknowledged and
// silenced separately.
func TestADeveloperConditionIsRecordedForBothAudiences(t *testing.T) {
	store := &fakeStore{}
	loop := loopOver(t, store, crashLoopingPod())

	round, err := loop.RoundOnce(context.Background())
	if err != nil {
		t.Fatalf("the round: %v", err)
	}
	audiences := map[string]bool{}
	for _, written := range store.written {
		for _, row := range written {
			if strings.HasPrefix(row.Signal, "workload.") {
				audiences[row.Audience] = true
				if row.Fingerprint == "" || row.Version < 1 {
					t.Errorf("every row carries its identity and its rule's version: %+v", row)
				}
			}
		}
	}
	if !audiences[string(signals.AudienceDeveloper)] || !audiences[string(signals.AudienceOperator)] {
		t.Fatalf("a developer condition reaches both audiences: %+v / %+v", audiences, round.Transitions)
	}
}

// An installation with no telemetry store has nowhere to record, and says so
// rather than half-recording a round the store-backed rules never ran in.
func TestARoundWithoutAStoreRecordsNothingAndSaysWhy(t *testing.T) {
	client := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRuntimeObjects(singleton()).
		WithStatusSubresource(&kitchenv1alpha1.Kitchen{}).
		Build()
	loop := &Loop{Client: client, Resolver: nowhere{}}

	round, err := loop.RoundOnce(context.Background())
	if err != nil {
		t.Fatalf("no store is not a failure: %v", err)
	}
	if round.Evaluated || round.Message == "" {
		t.Fatalf("a round with nowhere to write says so: %+v", round)
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := loop.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatalf("reading the singleton back: %v", err)
	}
	if kitchen.Status.Signals == nil || kitchen.Status.Signals.Message == "" {
		t.Errorf("the reason is on the object, where somebody will see it: %+v", kitchen.Status.Signals)
	}
	if kitchen.Status.Signals.LastEvaluated != nil {
		t.Errorf("nothing was evaluated, so nothing dates it: %+v", kitchen.Status.Signals)
	}
}

// Switched off is not the same as broken: no round, and no status pretending
// the platform is being watched.
func TestASwitchedOffLoopEvaluatesNothing(t *testing.T) {
	kitchen := singleton()
	kitchen.Spec.Observability.Signals.Enabled = new(bool)
	client := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRuntimeObjects(kitchen).
		WithStatusSubresource(&kitchenv1alpha1.Kitchen{}).
		Build()
	store := &fakeStore{}
	loop := &Loop{
		Client:   client,
		Resolver: nowhere{},
		store:    func(context.Context) (Store, error) { return store, nil },
	}

	round, err := loop.RoundOnce(context.Background())
	if err != nil {
		t.Fatalf("switched off is not a failure: %v", err)
	}
	if round.Evaluated || len(store.written) != 0 {
		t.Fatalf("nothing runs and nothing is written: %+v", round)
	}
}

// A write that failed must not leave the loop believing it recorded something.
// The next round seeds from the store again, which is the state the store is
// actually in.
func TestAFailedWriteIsRetriedFromTheStore(t *testing.T) {
	store := &fakeStore{insertErr: errors.New("dial tcp 10.0.0.1:8123: connect: connection refused")}
	loop := loopOver(t, store, crashLoopingPod())

	if _, err := loop.RoundOnce(context.Background()); err == nil {
		t.Fatal("a write that failed is an error the loop reports")
	}
	if loop.tracker != nil {
		t.Fatal("the round did not happen, so the loop must not think it did")
	}

	store.insertErr = nil
	round, err := loop.RoundOnce(context.Background())
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if round.Recorded == 0 {
		t.Fatalf("the condition still has to be recorded: %+v", round)
	}
}

// A round cannot start from an empty memory when the store holds open
// conditions: re-announcing every one of them is exactly what an inbox must
// never do on an operator restart.
func TestASeedThatCannotBeReadStopsTheRound(t *testing.T) {
	store := &fakeStore{openErr: errors.New("read timeout")}
	loop := loopOver(t, store, crashLoopingPod())

	if _, err := loop.RoundOnce(context.Background()); err == nil {
		t.Fatal("a seed that failed stops the round rather than opening everything again")
	}
	if len(store.written) != 0 {
		t.Fatalf("nothing is written on a round that never started: %+v", store.written)
	}
}

// What the store already holds open is not opened again — the case of a
// restarted operator, or a replica that has just won the lease.
func TestAnAlreadyOpenConditionIsNotReAnnounced(t *testing.T) {
	store := &fakeStore{}
	loop := loopOver(t, store, crashLoopingPod())
	first, err := loop.RoundOnce(context.Background())
	if err != nil {
		t.Fatalf("the first round: %v", err)
	}

	// A second loop, as a new leader would be, seeded from what the first one
	// wrote.
	store.open = signals.TransitionRows(first.Transitions)
	successor := loopOver(t, store, crashLoopingPod())
	round, err := successor.RoundOnce(context.Background())
	if err != nil {
		t.Fatalf("the successor's round: %v", err)
	}
	if round.Recorded != 0 {
		t.Fatalf("a new leader announces only what changed while it was not looking: %+v", round.Transitions)
	}
	if round.Open != first.Open {
		t.Errorf("it holds what the store held: %d, was %d", round.Open, first.Open)
	}
}

// The same round against a real ClickHouse, because the statements this loop's
// writes and its seed are made of are the kind that read perfectly and fail —
// a conditional TTL, an argMax per key, a column list that has drifted.
//
// Skipped unless a store is pointed at; CI points one at it. See
// internal/clickhouse's integration suite for the docker line.
func TestLoopRecordsTransitionsIntegration(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	if err := store.EnsureSignalsSchema(ctx, 30); err != nil {
		t.Fatalf("creating the signal history: %v", err)
	}
	if err := store.Exec(ctx, "TRUNCATE TABLE IF EXISTS "+clickhouse.SignalTransitionsTable); err != nil {
		t.Fatalf("clearing the signal history: %v", err)
	}

	loop := loopOver(t, store, crashLoopingPod())
	round, err := loop.RoundOnce(ctx)
	if err != nil {
		t.Fatalf("the round: %v", err)
	}
	if round.Recorded == 0 {
		t.Fatalf("the crash loop opened: %+v", round)
	}

	open, err := store.OpenSignalTransitions(ctx)
	if err != nil {
		t.Fatalf("reading back what is open: %v", err)
	}
	if len(open) != round.Open {
		t.Fatalf("what the store says is open is what the round left open: %d, want %d",
			len(open), round.Open)
	}
	for _, row := range open {
		if row.Fingerprint == "" || row.Audience == "" || row.Version < 1 {
			t.Errorf("a recorded row keeps its identity through the store: %+v", row)
		}
		if row.OpenedAt.IsZero() {
			t.Errorf("when the platform first saw it is the point of recording it: %+v", row)
		}
	}

	// And a round in which the condition has gone resolves it, which the open
	// set has to stop naming.
	successor := loopOver(t, store)
	successor.tracker = loop.tracker
	if _, err := successor.RoundOnce(ctx); err != nil {
		t.Fatalf("the resolving round: %v", err)
	}
	stillOpen, err := store.OpenSignalTransitions(ctx)
	if err != nil {
		t.Fatalf("reading back what is open: %v", err)
	}
	for _, row := range stillOpen {
		if strings.HasPrefix(row.Signal, "workload.") {
			t.Errorf("the condition resolved and must not still read as open: %+v", row)
		}
	}
}

// integrationDatabase is this package's own database on the store under test.
//
// It is deliberately not the one the URL names. `go test ./...` runs packages
// in parallel, internal/clickhouse's suite reshapes every table's TTL in the
// database the URL names, and two suites altering one table at once is a
// failure that looks like a bug in whichever of them lost. The schema creates
// the database it is pointed at, so a name of our own costs nothing.
const integrationDatabase = "kitchen_detection"

// integrationStore resolves the store under test, or skips.
func integrationStore(t *testing.T) *clickhouse.Client {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("KITCHEN_CLICKHOUSE_URL"))
	if raw == "" {
		t.Skip("set KITCHEN_CLICKHOUSE_URL to run the store integration tests")
	}
	endpoint, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("KITCHEN_CLICKHOUSE_URL is not a URL: %v", err)
	}
	password, _ := endpoint.User.Password()
	return clickhouse.New(clickhouse.Config{
		Host:     endpoint.Hostname(),
		HTTPPort: endpoint.Port(),
		Database: integrationDatabase,
		Username: endpoint.User.Username(),
		Password: password,
	})
}
