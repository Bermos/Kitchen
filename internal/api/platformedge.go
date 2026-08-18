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
	"net/http"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/flows"
	"github.com/Bermos/Kitchen/internal/signals"
)

// The edge, across every project: what the platform's front door served, and
// whether the door itself is in one piece.
//
// The traffic half is the same request pipeline the environment page reads,
// asked without a project — which is the only way to ask it. A rate is per the
// platform, a p95 is a merge over every project's states, and neither can be
// re-derived by adding up per-project rows: summing counts works, summing
// percentiles does not.
//
// The other half is what an operator checks when the numbers look wrong. The
// Gateway's own conditions, the tunnel in front of it, and the certificates
// behind it — including the ACME error verbatim, which is the single most
// useful string on the whole screen and the one thing no summary should ever
// paraphrase.

// How many rows each of the edge's tables carries. Ten is a table somebody
// reads rather than scrolls, and every one of these has a "sorted by" that
// decides which ten they are.
const defaultEdgeRows = 10

// edgeMinRequests is how much traffic a row needs before it can be ranked by
// error rate. Without it the worst-performing host on the platform is whichever
// scanner asked once and got a 404.
const edgeMinRequests = 20

// whatPlatformTraffic names the read in the dashboard's words, for the one
// failure that can come from three of the calls below.
const whatPlatformTraffic = "the platform traffic query"

// conditionTrue is how an unstructured object spells a condition that holds.
// cert-manager's are read as maps rather than through its Go types, so the
// status arrives as the string the API server serialised.
const conditionTrue = "True"

// certificateListGVK addresses cert-manager's kind as an unstructured object
// rather than through its Go types. That is how the operator addresses all of
// them, and the reason is the build: cert-manager's release cadence is not
// Kitchen's, and a typed dependency makes it one.
var certificateListGVK = schema.GroupVersionKind{
	Group:   "cert-manager.io",
	Version: "v1",
	Kind:    "CertificateList",
}

// platformEdge answers the Edge screen.
func (s *Server) platformEdge(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	since, until, err := windowFrom(req)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	rows, err := intParam(req, "limit", defaultEdgeRows)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	gateways, gatewayMessage := s.gatewayViews(ctx)
	body := edgeBody{
		Gateways:       gateways,
		GatewayMessage: gatewayMessage,
		Tunnel:         s.tunnelView(ctx),
		Certificates:   s.certificateViews(ctx),
	}

	// The traffic tables need the store; the edge's own objects do not. An
	// installation without telemetry still has a Gateway worth looking at, so
	// the answer degrades to those rather than to a 503.
	store, err := s.logStore(ctx)
	if err != nil {
		if !errors.Is(err, errNoLogStore) {
			s.writeError(w, err)
			return
		}
		body.TrafficMessage = noStoreMessage
		writeJSON(w, http.StatusOK, body)
		return
	}
	if what, err := s.fillEdgeTraffic(ctx, store, &body, clickhouse.PlatformRequestsQuery{
		Since: since,
		Until: until,
		Limit: rows,
	}); err != nil {
		s.writeStoreError(w, err, what)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// edgeBody is the Edge screen.
type edgeBody struct {
	// Requests is the platform's headline: everything that entered in the
	// window, however it was attributed.
	Requests clickhouse.PlatformRequests `json:"requests"`

	// The four rankings. They are separate fields rather than one table with a
	// sort parameter because the operator reads them together: the busiest
	// routes explain the load, and the worst ones explain the complaints.
	TopRoutes      []clickhouse.EdgeEntry `json:"topRoutes"`
	WorstRoutes    []clickhouse.EdgeEntry `json:"worstRoutes"`
	TopHosts       []clickhouse.EdgeEntry `json:"topHosts"`
	WorstHosts     []clickhouse.EdgeEntry `json:"worstHosts"`
	LatencyLeaders []clickhouse.EdgeEntry `json:"latencyLeaders"`

	// Unrouted is the bucket of hosts that reached the edge which the platform
	// never published: a stale DNS record, a scanner, or a custom domain whose
	// object was removed while its record was not.
	Unrouted []clickhouse.UnroutedHost `json:"unrouted"`

	Gateways     []edgeGatewayView `json:"gateways"`
	Tunnel       *edgeTunnelView   `json:"tunnel,omitempty"`
	Certificates certificateTable  `json:"certificates"`

	// GatewayMessage says why the Gateway list is empty, and is empty when the
	// platform genuinely has no Gateway.
	//
	// It is the difference between the two readings of an empty list, and on
	// this screen that difference is the whole answer: no Gateway means nothing
	// this platform publishes is reachable, which is the strongest claim the
	// health strip makes, and a List that failed produces the same empty slice
	// while proving nothing at all.
	GatewayMessage string `json:"gatewayMessage,omitempty"`

	// TrafficMessage says why the traffic tables are empty, and is empty when
	// they are simply empty.
	TrafficMessage string `json:"trafficMessage,omitempty"`
}

// fillEdgeTraffic reads the store's half of the screen, answering the name of
// the read that failed alongside the failure.
func (s *Server) fillEdgeTraffic(
	ctx context.Context,
	store logReader,
	body *edgeBody,
	query clickhouse.PlatformRequestsQuery,
) (string, error) {
	requests, err := store.PlatformRequests(ctx, query)
	if err != nil {
		return whatPlatformTraffic, err
	}
	body.Requests = requests

	// Five rankings of one window. Each is its own read because the sort
	// decides which rows survive the limit: the ten busiest routes and the ten
	// that fail most are rarely the same ten, and taking one from the other
	// would only ever be a guess.
	breakdowns := []struct {
		into   *[]clickhouse.EdgeEntry
		by     string
		sortBy string
		// floor drops rows too quiet to rank, which only a ratio needs.
		floor uint64
	}{
		{&body.TopRoutes, clickhouse.EdgeByRoute, clickhouse.RouteSortRequests, 0},
		{&body.WorstRoutes, clickhouse.EdgeByRoute, clickhouse.RouteSortErrorRate, edgeMinRequests},
		{&body.TopHosts, clickhouse.EdgeByHost, clickhouse.RouteSortRequests, 0},
		{&body.WorstHosts, clickhouse.EdgeByHost, clickhouse.RouteSortErrorRate, edgeMinRequests},
		{&body.LatencyLeaders, clickhouse.EdgeByEnvironment, clickhouse.RouteSortLatency, edgeMinRequests},
	}
	for _, breakdown := range breakdowns {
		entries, err := store.EdgeBreakdown(ctx, clickhouse.EdgeBreakdownQuery{
			Since:       query.Since,
			Until:       query.Until,
			By:          breakdown.by,
			SortBy:      breakdown.sortBy,
			MinRequests: breakdown.floor,
			Limit:       query.Limit,
		})
		if err != nil {
			return "the edge breakdown query", err
		}
		*breakdown.into = itemsOf(entries)
	}

	// The unrouted bucket is read over its own window rather than the screen's:
	// the question is whether a host has been asking *continuously*, which an
	// arbitrary window the operator dragged cannot answer.
	//
	// It is over-fetched by the number of names the platform published, because
	// the rows dropped below are dropped after the limit: the dashboard's own
	// hostname is usually the busiest name in the bucket, so a table filtered
	// down from exactly ten rows would answer with three.
	published := s.publishedHosts(ctx)
	unrouted, err := store.UnroutedHosts(ctx, clickhouse.PlatformRequestsQuery{
		Since: time.Now().UTC().Add(-signals.UnroutedWindow),
		Limit: query.Limit + published.Len(),
	})
	if err != nil {
		return "the unrouted host query", err
	}
	body.Unrouted = itemsOf(unroutedRows(unrouted, published, query.Limit))
	return "", nil
}

// publishedHosts is every hostname the platform published, which is what tells
// the two halves of the store's unattributed bucket apart.
//
// A request row is attributed by looking its host up in the routes that carry
// a project, so the platform's own surfaces — the dashboard, the API, the
// identity provider, all published by routes that carry none — land in the
// same bucket a stale DNS record does. The table claims those rows were never
// published, and for the dashboard's own URL that is simply untrue.
//
// A listing that failed leaves the bucket as the store returned it. The table
// then over-reports, which is the behaviour an operator can see through; the
// alternative, an empty table, would claim the edge served nothing it could
// not place.
func (s *Server) publishedHosts(ctx context.Context) flows.HostSet {
	routes := &gatewayv1.HTTPRouteList{}
	if err := s.reader().List(ctx, routes); err != nil {
		s.log().Error(err, "cannot read the platform's routes to filter the unrouted bucket")
		return flows.HostSet{}
	}
	return flows.PublishedHosts(routes.Items)
}

// unroutedRows drops the published hosts and trims what is left to the limit
// the caller asked for.
func unroutedRows(
	hosts []clickhouse.UnroutedHost,
	published flows.HostSet,
	limit int,
) []clickhouse.UnroutedHost {
	rows := make([]clickhouse.UnroutedHost, 0, len(hosts))
	for _, host := range hosts {
		if published.Covers(host.Host) {
			continue
		}
		rows = append(rows, host)
		if limit > 0 && len(rows) == limit {
			break
		}
	}
	return rows
}

// edgeGatewayView is one Gateway and its listeners, as the API server reports
// them.
//
// Programmed is the condition that matters and the one that is easiest to
// misread: cloudflared does not remove the need for a LoadBalancer address,
// only for it to be routable, so a tunnelled platform with no address reports
// Programmed=False / AddressNotAssigned and never serves anything.
type edgeGatewayView struct {
	Namespace  string             `json:"namespace"`
	Name       string             `json:"name"`
	Class      string             `json:"class,omitempty"`
	Addresses  []string           `json:"addresses,omitempty"`
	Programmed bool               `json:"programmed"`
	Accepted   bool               `json:"accepted"`
	Message    string             `json:"message,omitempty"`
	Listeners  []edgeListenerView `json:"listeners,omitempty"`
}

// edgeListenerView is one listener: which port it serves and how many routes
// attached to it, which is where a route that was refused shows up as an
// absence.
type edgeListenerView struct {
	Name           string `json:"name"`
	Port           int32  `json:"port"`
	Protocol       string `json:"protocol"`
	AttachedRoutes int32  `json:"attachedRoutes"`
	Programmed     bool   `json:"programmed"`
	Message        string `json:"message,omitempty"`
}

// gatewayViews reads the Gateways, answering the list and — where it is empty
// for a reason other than there being none — the sentence that says why.
//
// A read that failed must never come back as an empty list alone. Every
// consumer of this field reads "no Gateway" as "nothing on this platform is
// published", which is a definite and alarming claim, and a List that was
// refused proves none of it. So the failure travels with the list, exactly as
// certificateViews does it.
//
// A cluster whose Gateway API CRDs are not installed still answers rather than
// failing: the traffic numbers are worth showing, and the Kitchen singleton's
// own condition already says the Gateway is not programmed.
func (s *Server) gatewayViews(ctx context.Context) ([]edgeGatewayView, string) {
	list := &gatewayv1.GatewayList{}
	if err := s.reader().List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return []edgeGatewayView{}, "the Gateway API kinds are not installed in this cluster, " +
				"so there is no Gateway to publish anything"
		}
		s.log().Error(err, "cannot read the platform's Gateways")
		return []edgeGatewayView{}, "the platform's Gateways could not be read: " + err.Error()
	}
	views := make([]edgeGatewayView, 0, len(list.Items))
	for i := range list.Items {
		gateway := &list.Items[i]
		view := edgeGatewayView{
			Namespace: gateway.Namespace,
			Name:      gateway.Name,
			Class:     string(gateway.Spec.GatewayClassName),
		}
		for _, address := range gateway.Status.Addresses {
			view.Addresses = append(view.Addresses, address.Value)
		}
		for _, condition := range gateway.Status.Conditions {
			switch condition.Type {
			case string(gatewayv1.GatewayConditionProgrammed):
				view.Programmed = condition.Status == metav1.ConditionTrue
				if !view.Programmed {
					view.Message = withReason(condition.Reason, condition.Message)
				}
			case string(gatewayv1.GatewayConditionAccepted):
				view.Accepted = condition.Status == metav1.ConditionTrue
				if !view.Accepted && view.Message == "" {
					view.Message = withReason(condition.Reason, condition.Message)
				}
			}
		}
		for _, listener := range gateway.Status.Listeners {
			view.Listeners = append(view.Listeners, newListenerView(gateway, listener))
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Namespace != views[j].Namespace {
			return views[i].Namespace < views[j].Namespace
		}
		return views[i].Name < views[j].Name
	})
	return views, ""
}

// newListenerView joins a listener's reported status to the spec it came from,
// which is where its port and protocol are.
func newListenerView(gateway *gatewayv1.Gateway, status gatewayv1.ListenerStatus) edgeListenerView {
	view := edgeListenerView{
		Name:           string(status.Name),
		AttachedRoutes: status.AttachedRoutes,
	}
	for _, listener := range gateway.Spec.Listeners {
		if listener.Name == status.Name {
			view.Port = int32(listener.Port)
			view.Protocol = string(listener.Protocol)
			break
		}
	}
	for _, condition := range status.Conditions {
		if condition.Type == string(gatewayv1.ListenerConditionProgrammed) {
			view.Programmed = condition.Status == metav1.ConditionTrue
			if !view.Programmed {
				view.Message = withReason(condition.Reason, condition.Message)
			}
		}
	}
	return view
}

// edgeTunnelView is cloudflared, where it runs at all.
type edgeTunnelView struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Desired   int32  `json:"desired"`
	Ready     int32  `json:"ready"`
	Available int32  `json:"available"`
	// Restarts is the tunnel's own, which is what tells "restarted" from
	// "flapping": a tunnel that cannot hold a connection restarts repeatedly
	// while its Deployment reports a replica that is nominally available.
	Restarts int32  `json:"restarts"`
	Healthy  bool   `json:"healthy"`
	Message  string `json:"message,omitempty"`
}

// tunnelView reads the cloudflared Deployment, or nothing where the platform
// runs without a tunnel — which is the ordinary case and not a fault.
func (s *Server) tunnelView(ctx context.Context) *edgeTunnelView {
	deployments := &appsv1.DeploymentList{}
	if err := s.listPlatform(ctx, deployments, map[string]string{
		labelPartOf:    partOfKitchen,
		labelComponent: componentCloudflared,
	}); err != nil {
		s.log().Error(err, "cannot read the tunnel's Deployment")
		return nil
	}
	if len(deployments.Items) == 0 {
		return nil
	}

	tunnel := &deployments.Items[0]
	view := &edgeTunnelView{
		Name:      tunnel.Name,
		Namespace: tunnel.Namespace,
		Desired:   replicasOrOne(tunnel.Spec.Replicas),
		Ready:     tunnel.Status.ReadyReplicas,
		Available: tunnel.Status.AvailableReplicas,
	}
	view.Healthy = view.Available >= view.Desired

	pods := &corev1.PodList{}
	if err := s.listPlatform(ctx, pods, map[string]string{
		labelPartOf:    partOfKitchen,
		labelComponent: componentCloudflared,
	}); err != nil {
		return view
	}
	for i := range pods.Items {
		for j := range pods.Items[i].Status.ContainerStatuses {
			status := &pods.Items[i].Status.ContainerStatuses[j]
			view.Restarts += status.RestartCount
			if view.Message == "" {
				view.Message = containerMessage(status)
			}
		}
	}
	return view
}

// certificateTable is the certificates the platform's TLS rests on: the
// wildcard the shared Gateway serves every generated URL under, and one per
// custom domain.
type certificateTable struct {
	Items []certificateView `json:"items"`
	// Message says why the table is empty. cert-manager not being installed is
	// a supported configuration — TLS mode none, or a certificate supplied by
	// hand — and is not the same as having no certificates.
	Message string `json:"message,omitempty"`
}

// certificateView is one certificate, in the fields an operator judges it by.
type certificateView struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	DNSNames  []string `json:"dnsNames,omitempty"`
	Ready     bool     `json:"ready"`
	// NotAfter is when it stops being valid, and DaysToExpiry the same fact in
	// the unit the decision is made in. Both are absent for a certificate that
	// has never been issued.
	NotAfter     *time.Time `json:"notAfter,omitempty"`
	DaysToExpiry *float64   `json:"daysToExpiry,omitempty"`
	RenewalTime  *time.Time `json:"renewalTime,omitempty"`
	// Message is the Ready condition's, verbatim: for a stuck ACME order it is
	// the error the CA returned, which is the one string that says what to fix.
	Message string `json:"message,omitempty"`
	// Issuing is the message on the Issuing condition where a renewal is in
	// progress. A renewal that keeps failing reports itself there while Ready
	// stays true on the still-valid old certificate, so this is the only place
	// a stuck renewal says so.
	Issuing string `json:"issuing,omitempty"`
}

// certificateViews reads cert-manager's Certificates as unstructured objects.
func (s *Server) certificateViews(ctx context.Context) certificateTable {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(certificateListGVK)
	if err := s.reader().List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return certificateTable{
				Items:   []certificateView{},
				Message: "cert-manager's Certificate kind is not installed in this cluster",
			}
		}
		s.log().Error(err, "cannot read the platform's certificates")
		return certificateTable{
			Items:   []certificateView{},
			Message: "the platform's certificates could not be read: " + err.Error(),
		}
	}

	now := time.Now().UTC()
	table := certificateTable{Items: make([]certificateView, 0, len(list.Items))}
	for i := range list.Items {
		table.Items = append(table.Items, newCertificateView(&list.Items[i], now))
	}
	sort.Slice(table.Items, func(i, j int) bool {
		if table.Items[i].Namespace != table.Items[j].Namespace {
			return table.Items[i].Namespace < table.Items[j].Namespace
		}
		return table.Items[i].Name < table.Items[j].Name
	})
	return table
}

func newCertificateView(object *unstructured.Unstructured, now time.Time) certificateView {
	view := certificateView{Namespace: object.GetNamespace(), Name: object.GetName()}
	view.DNSNames, _, _ = unstructured.NestedStringSlice(object.Object, "spec", "dnsNames")

	if notAfter := nestedTime(object, "status", "notAfter"); !notAfter.IsZero() {
		days := notAfter.Sub(now).Hours() / 24
		view.NotAfter, view.DaysToExpiry = &notAfter, &days
	}
	if renewal := nestedTime(object, "status", "renewalTime"); !renewal.IsZero() {
		view.RenewalTime = &renewal
	}

	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		conditionType, _ := condition["type"].(string)
		status, _ := condition["status"].(string)
		reason, _ := condition["reason"].(string)
		message, _ := condition["message"].(string)
		switch conditionType {
		case "Ready":
			view.Ready = status == conditionTrue
			view.Message = withReason(reason, message)
		case "Issuing":
			if status == conditionTrue {
				view.Issuing = withReason(reason, message)
			}
		}
	}
	if view.Ready {
		// A healthy certificate's Ready message is "Certificate is up to date
		// and has not expired", which is the table's own answer already.
		view.Message = ""
	}
	return view
}

// nestedTime reads an RFC 3339 field off an unstructured object, or the zero
// time where it is absent or unparseable.
func nestedTime(object *unstructured.Unstructured, fields ...string) time.Time {
	value, found, err := unstructured.NestedString(object.Object, fields...)
	if !found || err != nil {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// componentCloudflared is the tunnel's component label, which is how it is
// found: the Deployment's own name carries the release name.
const componentCloudflared = "cloudflared"
