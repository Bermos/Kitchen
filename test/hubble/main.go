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

// Command hubble is the reading half of stage 0 in docs/OBSERVABILITY.md. It
// follows Hubble Relay and reports what the shared Gateway's Envoy told Hubble
// about the HTTP requests hack/check-hubble-l7.sh has just sent through it.
//
// It exists because the observability design rests on one fact that could not
// be established without a cluster: that Gateway API traffic produces L7 flow
// records carrying method, URL, status, protocol and a non-zero latency, with
// no CiliumNetworkPolicy anywhere in the cluster. That fact is asserted here,
// and this process exits non-zero when it does not hold — the design's whole
// request pipeline hangs off it, and its named fallback (Envoy access logs
// injected through CiliumEnvoyConfig) is a much worse design to fall back to
// unknowingly.
//
// Everything else printed is an observation and never a failure: the open
// questions in §9 are questions the design flagged rather than guessed, and a
// job that failed on them would be asserting an answer nobody has. They are
// printed so a person can read the answer off a CI run.
//
// It dials Relay the way the operator's own follower does (internal/flows), so
// this is also the first CI that exercises the flow pipeline at all.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	observerpb "github.com/cilium/cilium/api/v1/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The labels the harness gives its requests, taken from the path segment after
// the match fragment: hack/hubble-l7-probe.sh sends /stage0/<label>.
const (
	labelOK    = "ok"
	labelDead  = "dead"
	labelTrace = "trace"
	labelGRPC  = "grpc"
)

// tailFlows is how many matching flows the diagnostic tail prints. Enough to
// see every request the probe sent twice over, short enough to read.
const tailFlows = 24

var (
	requestType  = flowpb.L7FlowType_REQUEST.String()
	responseType = flowpb.L7FlowType_RESPONSE.String()
)

// options are the knobs the harness sets. The defaults are what a developer
// running this by hand against a port-forwarded Relay wants.
type options struct {
	relay   string
	since   time.Duration
	window  time.Duration
	match   string
	traceID string
	summary string
	observe bool
}

func parseOptions() options {
	var o options
	flag.StringVar(&o.relay, "relay", "127.0.0.1:4245", "host:port of Hubble Relay's gRPC endpoint")
	flag.DurationVar(&o.since, "since", 10*time.Minute, "how far back to ask Relay for buffered flows")
	flag.DurationVar(&o.window, "window", 45*time.Second, "how long to hold the stream open")
	flag.StringVar(&o.match, "match", "/stage0/", "the URL fragment identifying the harness's requests")
	flag.StringVar(&o.traceID, "trace-id", "", "the trace id the harness sent in a traceparent header")
	flag.StringVar(&o.summary, "summary", "", "append a markdown copy of the report to this file")
	flag.BoolVar(&o.observe, "observe", false, "only report what is on the stream, asserting nothing")
	flag.Parse()
	return o
}

// probe is one HTTP transaction as Hubble reported it — the fields §3.2 wants
// to write a request row from, plus the attribution around them.
type probe struct {
	label       string
	kind        string
	method      string
	rawURL      string
	protocol    string
	code        uint32
	latencyNS   uint64
	headers     []string
	traceID     string
	verdict     string
	source      string
	destination string
	node        string
	at          time.Time
}

// observations is everything one streaming window saw.
type observations struct {
	flows     int
	l7        int
	probes    []probe
	lost      map[string]uint64
	notices   int
	status    *observerpb.ServerStatusResponse
	statusErr error
}

// section is one block of the report. Both renderings are built from these, so
// the job log and the step summary can never say different things.
type section struct {
	title string
	lines []string
}

func main() {
	o := parseOptions()

	conn, err := grpc.NewClient(o.relay, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		bail("hubble relay unreachable at %s: %v", o.relay, err)
	}
	defer func() { _ = conn.Close() }()
	client := observerpb.NewObserverClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), o.window+time.Minute)
	defer cancel()

	seen, err := collect(ctx, client, o)
	if err != nil {
		bail("reading flows from %s: %v", o.relay, err)
	}
	seen.status, seen.statusErr = serverStatus(ctx, client)

	sections, held := report(seen, o)
	emit(sections, o.summary)
	if !held {
		os.Exit(1)
	}
}

func bail(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// collect holds one stream open for the window and keeps what crosses it.
//
// The request asks for flows since before the harness sent its traffic, so the
// probe can run to completion first and this can read the result out of
// Hubble's buffer: nothing here has to race the requests it is looking for.
// Follow stays on so anything still in flight arrives too.
func collect(ctx context.Context, client observerpb.ObserverClient, o options) (*observations, error) {
	streamCtx, cancel := context.WithTimeout(ctx, o.window)
	defer cancel()

	stream, err := client.GetFlows(streamCtx, &observerpb.GetFlowsRequest{
		Follow: true,
		Since:  timestamppb.New(time.Now().Add(-o.since)),
	})
	if err != nil {
		return nil, err
	}

	seen := &observations{lost: map[string]uint64{}}
	for {
		response, err := stream.Recv()
		if err != nil {
			// The window closing is how this ends, not a fault.
			if windowClosed(streamCtx, err) {
				return seen, nil
			}
			return nil, err
		}
		seen.absorb(response, o.match)
	}
}

// windowClosed says whether the stream ended because the window did. Both ends
// of the RPC enforce the deadline — gRPC hands it to the server too — so Relay
// can tear the stream down a moment before the local context notices, and the
// status code is what makes the distinction reliable. Getting this wrong turns
// every successful run into "reading flows failed".
func windowClosed(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, io.EOF) {
		return true
	}
	switch status.Code(err) {
	case codes.DeadlineExceeded, codes.Canceled:
		return true
	default:
		return false
	}
}

// absorb files one response: a loss notice, a flow with an HTTP record, or
// neither.
func (s *observations) absorb(response *observerpb.GetFlowsResponse, match string) {
	if lost := response.GetLostEvents(); lost != nil {
		s.lost[lost.GetSource().String()] += lost.GetNumEventsLost()
		s.notices++
		return
	}
	flow := response.GetFlow()
	if flow == nil {
		return
	}
	s.flows++
	http := flow.GetL7().GetHttp()
	if http == nil {
		return
	}
	s.l7++
	if !strings.Contains(http.GetUrl(), match) {
		return
	}
	s.probes = append(s.probes, newProbe(flow, http, match))
}

func (s *observations) byLabel(label string) []probe {
	matched := make([]probe, 0, len(s.probes))
	for _, p := range s.probes {
		if p.label == label {
			matched = append(matched, p)
		}
	}
	return matched
}

func newProbe(flow *flowpb.Flow, http *flowpb.HTTP, match string) probe {
	p := probe{
		label:       label(http.GetUrl(), match),
		kind:        flow.GetL7().GetType().String(),
		method:      http.GetMethod(),
		rawURL:      http.GetUrl(),
		protocol:    http.GetProtocol(),
		code:        http.GetCode(),
		latencyNS:   flow.GetL7().GetLatencyNs(),
		traceID:     flow.GetTraceContext().GetParent().GetTraceId(),
		verdict:     flow.GetVerdict().String(),
		node:        flow.GetNodeName(),
		source:      endpoint(flow.GetSource()),
		destination: endpoint(flow.GetDestination()),
		headers:     make([]string, 0, len(http.GetHeaders())),
	}
	for _, header := range http.GetHeaders() {
		p.headers = append(p.headers, header.GetKey()+": "+header.GetValue())
	}
	if at := flow.GetTime(); at != nil {
		p.at = at.AsTime()
	}
	return p
}

// label is the path segment the harness names a request by: everything between
// the match fragment and the next separator.
func label(raw, match string) string {
	index := strings.Index(raw, match)
	if index < 0 {
		return ""
	}
	rest := raw[index+len(match):]
	if cut := strings.IndexAny(rest, "/?#"); cut >= 0 {
		rest = rest[:cut]
	}
	return rest
}

// endpoint names a flow endpoint the way the operator's follower does, falling
// back to the identity labels — which is where "reserved:ingress" shows up, and
// so where the Gateway announces itself.
func endpoint(ep *flowpb.Endpoint) string {
	if ep == nil {
		return "unknown"
	}
	name := ep.GetPodName()
	if workloads := ep.GetWorkloads(); len(workloads) > 0 && workloads[0].GetName() != "" {
		name = workloads[0].GetName()
	}
	switch {
	case ep.GetNamespace() != "" && name != "":
		return ep.GetNamespace() + "/" + name
	case name != "":
		return name
	case len(ep.GetLabels()) > 0:
		return strings.Join(ep.GetLabels(), ",")
	default:
		return fmt.Sprintf("identity %d", ep.GetIdentity())
	}
}

func serverStatus(ctx context.Context, client observerpb.ObserverClient) (*observerpb.ServerStatusResponse, error) {
	statusCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return client.ServerStatus(statusCtx, &observerpb.ServerStatusRequest{})
}

// report builds every section and says whether the assertion held. In observe
// mode it makes no assertion at all: the harness runs it that way to dump
// recent flows when something else has already failed, and a FAIL banner over
// a dump of somebody else's traffic would be a lie.
func report(s *observations, o options) ([]section, bool) {
	if o.observe {
		return []section{contextSection(s, o), bufferSection(s), tailSection(s)}, true
	}
	proof, missing := assertVantagePoint(s)
	return []section{
		contextSection(s, o),
		vantageSection(proof, missing),
		deadBackendSection(s),
		traceContextSection(s, o),
		headerSection(s),
		bufferSection(s),
		tailSection(s),
	}, len(missing) == 0
}

// assertVantagePoint is the one thing this program is allowed to fail on: a
// response flow for the harness's plain request, carrying every field §3.2
// reads. It returns the best candidate it saw and what that candidate lacked,
// so a failure names the missing field rather than only the absence.
func assertVantagePoint(s *observations) (probe, []string) {
	var best probe
	found := false
	for _, p := range s.byLabel(labelOK) {
		if p.kind != responseType {
			continue
		}
		best, found = p, true
		if len(shortfall(p)) == 0 {
			return p, nil
		}
	}
	if !found {
		return probe{}, []string{
			"no L7 HTTP " + responseType + " flow for the probe request reached Relay",
			"the design's chosen vantage point (§3.1a) does not observe Gateway traffic here",
		}
	}
	return best, shortfall(best)
}

// shortfall lists the fields the request pipeline needs and this flow lacks.
func shortfall(p probe) []string {
	var missing []string
	if p.method != "GET" {
		missing = append(missing, fmt.Sprintf("l7.http.method is %q, not the GET that was sent", p.method))
	}
	if p.rawURL == "" {
		missing = append(missing, "l7.http.url is empty — §3.2 keys host attribution on it")
	}
	if p.code != 200 {
		missing = append(missing, fmt.Sprintf("l7.http.code is %d, not the 200 the backend answered", p.code))
	}
	if p.protocol == "" {
		missing = append(missing, "l7.http.protocol is empty")
	}
	if p.latencyNS == 0 {
		missing = append(missing, "l7.latency_ns is zero — there is no latency signal to build p95 from")
	}
	return missing
}

func contextSection(s *observations, o options) section {
	lines := []string{
		fmt.Sprintf("relay          %s", o.relay),
		fmt.Sprintf("window         %s of follow, over flows since %s ago", o.window, o.since),
		fmt.Sprintf("flows          %d in the window, %d carrying an HTTP record", s.flows, s.l7),
		fmt.Sprintf("matching       %d of them name %s", len(s.probes), o.match),
	}
	if s.flows == 0 {
		lines = append(lines, "no flow of any kind arrived: Relay itself is the suspect, not the Gateway")
	}
	return section{title: "Hubble stage 0 — reading the edge", lines: lines}
}

func vantageSection(proof probe, missing []string) section {
	s := section{title: "ASSERTION — the Gateway's Envoy emits L7 HTTP flows (§3.1a)"}
	if len(missing) == 0 {
		s.lines = append(s.lines, "PASS   every field the request pipeline reads is present, with no policy in the cluster")
	} else {
		s.lines = append(s.lines, "FAIL   the design's one load-bearing unverified fact does not hold here")
		for _, reason := range missing {
			s.lines = append(s.lines, "       "+reason)
		}
		s.lines = append(s.lines, "       the named fallback is Envoy access logs via CiliumEnvoyConfig (§3.1b)")
	}
	return section{title: s.title, lines: append(s.lines, describe(proof)...)}
}

// describe prints the flow the assertion was decided on, field by field, in
// the names the design uses for them.
func describe(p probe) []string {
	if p.kind == "" {
		return []string{"", "no candidate flow to describe"}
	}
	lines := []string{
		"",
		fmt.Sprintf("l7.type            %s", p.kind),
		fmt.Sprintf("l7.http.method     %s", p.method),
		fmt.Sprintf("l7.http.url        %s", p.rawURL),
		fmt.Sprintf("l7.http.code       %d", p.code),
		fmt.Sprintf("l7.http.protocol   %s", p.protocol),
		fmt.Sprintf("l7.latency_ns      %d (%s)", p.latencyNS, time.Duration(p.latencyNS)),
		fmt.Sprintf("verdict            %s", p.verdict),
		fmt.Sprintf("source             %s", p.source),
		fmt.Sprintf("destination        %s", p.destination),
		fmt.Sprintf("node               %s", p.node),
	}
	if host := authority(p.rawURL); host != "" {
		lines = append(lines, fmt.Sprintf("authority in url   %s — §3.2 can attribute on it", host))
	} else {
		lines = append(lines,
			"authority in url   absent: the url is a path alone.",
			"                   §3.2 attributes a request by the Host header taken from",
			"                   l7.http.url, so the follower would need another source.")
	}
	return lines
}

func authority(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// deadBackendSection answers §9's "requests Envoy answers itself": the harness
// curls a route whose Service has no endpoints at all.
func deadBackendSection(s *observations) section {
	out := section{title: "OBSERVATION — requests Envoy answers itself (§9)"}
	dead := s.byLabel(labelDead)
	responses, requests := split(dead)

	switch {
	case len(dead) == 0:
		out.lines = append(out.lines,
			"no L7 flow of any kind for the route with no backend endpoints.",
			"Reading: Envoy's own 5xx is invisible at this vantage point — an environment",
			"whose pods are all gone would look like vanished traffic, not like errors, and",
			"the env.no-backend signal (§7) would have to be computed from workload state.")
	case len(responses) > 0:
		out.lines = append(out.lines,
			fmt.Sprintf("%d response flow(s) and %d request flow(s) arrived.", len(responses), len(requests)),
			"Reading: Envoy's self-generated answer is recorded like any other response, so",
			"the request pipeline sees it as a real status and env.no-backend can be read",
			"off request rows.")
	default:
		out.lines = append(out.lines,
			fmt.Sprintf("%d request flow(s) arrived and no response flow.", len(requests)),
			"Reading: only the upstream-facing half is observable. The status Envoy sent the",
			"client is not in the stream, so these requests would be counted but never")
		out.lines = append(out.lines, "attributed a status.")
	}
	return section{title: out.title, lines: append(out.lines, list(dead)...)}
}

// traceContextSection answers §9's "trace context at the edge": the harness
// sends a W3C traceparent and this looks for it on the flow.
func traceContextSection(s *observations, o options) section {
	out := section{title: "OBSERVATION — trace context at the edge (§9)"}
	traced := s.byLabel(labelTrace)
	carried := ""
	for _, p := range traced {
		if p.traceID != "" {
			carried = p.traceID
			break
		}
	}
	switch {
	case len(traced) == 0:
		out.lines = append(out.lines, "no flow for the traceparent request; nothing to read.")
	case carried == "":
		out.lines = append(out.lines,
			fmt.Sprintf("flow.trace_context is empty on all %d flow(s), though the request carried", len(traced)),
			fmt.Sprintf("traceparent with trace id %s.", o.traceID),
			"Reading: http_requests.trace_id cannot be filled from the edge; it stays reserved",
			"and is only ever joined from spans an instrumented application sent itself.")
	case carried == o.traceID:
		out.lines = append(out.lines,
			fmt.Sprintf("flow.trace_context.parent.trace_id is %s — the id the request carried.", carried),
			"Reading: http_requests.trace_id can be filled at the edge, so an instrumented",
			"application's requests link to its traces with no work on the write side.")
	default:
		out.lines = append(out.lines,
			fmt.Sprintf("flow.trace_context.parent.trace_id is %s but the request sent %s.", carried, o.traceID),
			"Reading: a trace id is populated, but not the incoming one — worth understanding",
			"before anything depends on it.")
	}
	return section{title: out.title, lines: append(out.lines, list(traced)...)}
}

// headerSection answers §9's gRPC question the only way it can be answered
// cheaply: whether Hubble's header list is populated at all for Gateway
// traffic. If it is empty, grpc-status is not obtainable from this vantage
// point at any cardinality cost, and §3.4's footnote stands.
func headerSection(s *observations) section {
	out := section{title: "OBSERVATION — gRPC status and header capture (§3.4, §9)"}
	withHeaders := 0
	grpcStatus := ""
	keys := map[string]int{}
	for _, p := range s.probes {
		if len(p.headers) > 0 {
			withHeaders++
		}
		for _, header := range p.headers {
			key := strings.ToLower(strings.SplitN(header, ":", 2)[0])
			keys[key]++
			if key == "grpc-status" {
				grpcStatus = header
			}
		}
	}
	switch {
	case withHeaders == 0:
		out.lines = append(out.lines,
			fmt.Sprintf("l7.http.headers is empty on all %d matching flow(s).", len(s.probes)),
			"Reading: no header reaches the follower, so grpc-status cannot be read here at",
			"any price. §3.4's footnote — gRPC application errors are not counted — stands,",
			"and the cardinality/privacy question the design worried about does not arise.")
	case grpcStatus != "":
		out.lines = append(out.lines,
			fmt.Sprintf("headers are populated on %d flow(s), including %q.", withHeaders, grpcStatus),
			"Reading: grpc-status is obtainable; what remains is the cardinality and privacy",
			"cost of keeping headers, which §9 says to weigh rather than assume.")
	default:
		out.lines = append(out.lines,
			fmt.Sprintf("headers are populated on %d flow(s) but carry no grpc-status:", withHeaders),
			"  "+strings.Join(sortedKeys(keys), ", "),
			"Reading: the header list exists, so the question narrows to whether the status a",
			"gRPC call ends with is among the headers Cilium records.")
	}
	out.lines = append(out.lines, "", "protocols reported: "+tally(s.probes))
	out.lines = append(out.lines, list(s.byLabel(labelGRPC))...)
	return out
}

// bufferSection is §8's ask: guidance for hubble.eventBufferCapacity, which is
// a question about how many seconds of traffic the default 4095-flow ring holds
// at a Kitchen-realistic rate.
func bufferSection(s *observations) section {
	out := section{title: "MEASUREMENT — Hubble buffer and loss (§3.2, §8)"}
	if s.notices == 0 {
		out.lines = append(out.lines, "lost-event notices  none in the window")
	} else {
		out.lines = append(out.lines, fmt.Sprintf("lost-event notices  %d", s.notices))
		for _, source := range sortedKeys(countsAsInts(s.lost)) {
			out.lines = append(out.lines, fmt.Sprintf("  %-26s %d events", source, s.lost[source]))
		}
	}
	if s.statusErr != nil {
		out.lines = append(out.lines, fmt.Sprintf("server status       unavailable: %v", s.statusErr))
		return out
	}
	st := s.status
	out.lines = append(out.lines,
		fmt.Sprintf("ring buffer         %d flows cached of %d capacity", st.GetNumFlows(), st.GetMaxFlows()),
		fmt.Sprintf("seen since start    %d flows in %s", st.GetSeenFlows(), time.Duration(st.GetUptimeNs())),
		fmt.Sprintf("rate                %.1f flows/s over the last minute", st.GetFlowsRate()),
		fmt.Sprintf("version             %s", st.GetVersion()),
	)
	if rate := st.GetFlowsRate(); rate > 0 && st.GetMaxFlows() > 0 {
		holds := time.Duration(float64(st.GetMaxFlows())/rate) * time.Second
		out.lines = append(out.lines,
			"",
			fmt.Sprintf("guidance            at %.1f flows/s this buffer holds about %s of history.", rate, holds),
			"                    A follower that reconnects more slowly than that loses flows",
			"                    silently unless eventBufferCapacity is raised.")
	}
	return out
}

func tailSection(s *observations) section {
	out := section{title: "Every matching flow in the window"}
	if len(s.probes) == 0 {
		out.lines = append(out.lines, "none")
		return out
	}
	out.lines = list(s.probes)
	return out
}

// list renders flows one per line, newest last, capped so a busy window cannot
// bury the report.
func list(probes []probe) []string {
	if len(probes) == 0 {
		return nil
	}
	from := 0
	if len(probes) > tailFlows {
		from = len(probes) - tailFlows
	}
	lines := make([]string, 0, len(probes)-from+1)
	lines = append(lines, "")
	for _, p := range probes[from:] {
		lines = append(lines, fmt.Sprintf("%s  %-8s %-6s %-4s %3d %-8s %8s  %s",
			p.at.Format("15:04:05.000"), p.kind, p.label, p.method, p.code, p.protocol,
			time.Duration(p.latencyNS), p.rawURL))
	}
	return lines
}

func split(probes []probe) (responses, requests []probe) {
	for _, p := range probes {
		switch p.kind {
		case responseType:
			responses = append(responses, p)
		case requestType:
			requests = append(requests, p)
		}
	}
	return responses, requests
}

func tally(probes []probe) string {
	counts := map[string]int{}
	for _, p := range probes {
		if p.protocol != "" {
			counts[p.protocol]++
		}
	}
	if len(counts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(counts))
	for _, protocol := range sortedKeys(counts) {
		parts = append(parts, fmt.Sprintf("%s (%d)", protocol, counts[protocol]))
	}
	return strings.Join(parts, ", ")
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func countsAsInts(counts map[string]uint64) map[string]int {
	out := make(map[string]int, len(counts))
	for key, value := range counts {
		out[key] = int(value)
	}
	return out
}

// emit writes the report to the job log, and to the step summary when CI gave
// us one, so the answers to the open questions survive the log's retention.
func emit(sections []section, summaryPath string) {
	for _, s := range sections {
		_, _ = fmt.Fprintf(os.Stdout, "\n== %s ==\n", s.title)
		for _, line := range s.lines {
			_, _ = fmt.Fprintf(os.Stdout, "   %s\n", line)
		}
	}
	if summaryPath == "" {
		return
	}
	file, err := os.OpenFile(summaryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "could not write the step summary: %v\n", err)
		return
	}
	defer func() { _ = file.Close() }()
	for _, s := range sections {
		_, _ = fmt.Fprintf(file, "\n### %s\n\n```\n", s.title)
		for _, line := range s.lines {
			_, _ = fmt.Fprintf(file, "%s\n", line)
		}
		_, _ = fmt.Fprint(file, "```\n")
	}
}
