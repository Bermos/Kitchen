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

package clickhouse

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The three questions that turn a page of lines into an answer, all asked over
// the same LogSelection the lines were asked over:
//
//   - the histogram says *when* — the shape of the window, so a spike is seen
//     rather than scrolled to;
//   - the facets say *what else is in here* — the distinct values of a column
//     with their counts, so a user narrows by clicking rather than by knowing
//     the column exists;
//   - the patterns say *what is actually being said* — the lines collapsed to
//     templates, so 14,021 lines become a handful of shapes.
//
// All three carry caller-written query text (the `where` escape hatch) and so
// run under the same read-only settings and execution cap as the lines do.

// Histogram bucket counts, and the ladder of bucket widths a window is
// quantised to. The ladder exists so that panning the window does not restripe
// the chart at some arbitrary width like 47 seconds.
const (
	// DefaultHistogramBuckets is how many bars a histogram is drawn with when
	// the caller does not say, and MaxHistogramBuckets the ceiling: the answer
	// is read whole and drawn in a few hundred pixels.
	DefaultHistogramBuckets = 60
	MaxHistogramBuckets     = 480
)

var bucketLadder = []int{
	1, 2, 5, 10, 15, 30,
	60, 120, 300, 600, 900, 1800,
	3600, 7200, 10800, 21600, 43200,
	86400, 172800, 604800,
}

// LogHistogramQuery counts the selection's lines over time.
type LogHistogramQuery struct {
	LogSelection
	// Buckets is how many bars are wanted. The width is rounded up to the
	// next value on the ladder, so the answer usually has fewer.
	Buckets int
}

// LogBucket is one bar: how many lines fell in it, and how many of those were
// bad news. The severities are split out here rather than left to a second
// query because "when did the errors start" is the question the chart is for.
type LogBucket struct {
	Start    time.Time `json:"start"`
	Count    uint64    `json:"count"`
	Errors   uint64    `json:"errors"`
	Warnings uint64    `json:"warnings"`
}

// LogHistogram is the shape of a window: every bucket in it, including the
// empty ones, because a gap is information.
type LogHistogram struct {
	Start         time.Time   `json:"start"`
	End           time.Time   `json:"end"`
	BucketSeconds int         `json:"bucketSeconds"`
	Buckets       []LogBucket `json:"buckets"`
	Total         uint64      `json:"total"`
}

// LogHistogram counts the selection into buckets across its window.
//
// A selection with no start is "everything retained", which is not a span this
// can bucket, so the store is asked what the span actually is first. That is
// one cheap aggregate over the same predicate, and it is the difference between
// a chart of the data and a chart of an assumed 24 hours.
func (c *Client) LogHistogram(ctx context.Context, query LogHistogramQuery) (LogHistogram, error) {
	buckets := query.Buckets
	if buckets < 1 {
		buckets = DefaultHistogramBuckets
	}
	if buckets > MaxHistogramBuckets {
		buckets = MaxHistogramBuckets
	}

	start, end, err := c.logWindow(ctx, query.LogSelection)
	if err != nil {
		return LogHistogram{}, err
	}
	if start.IsZero() || !end.After(start) {
		// Nothing matched, so there is no window to draw. An empty histogram
		// is the honest answer; inventing one would draw a flat line over a
		// span that holds no lines at all.
		return LogHistogram{BucketSeconds: bucketLadder[0], Buckets: []LogBucket{}}, nil
	}

	width := bucketSeconds(end.Sub(start), buckets)
	// ClickHouse's toStartOfInterval counts seconds from the Unix epoch, so
	// the buckets this fills in have to as well. Go's Truncate counts from
	// year 1, which agrees for widths that divide a day and silently does not
	// for the two-day and one-week rungs of the ladder.
	start = time.Unix(start.Unix()-start.Unix()%int64(width), 0).UTC()

	where, params, err := query.whereClause()
	if err != nil {
		return LogHistogram{}, err
	}
	params["bucket"] = strconv.Itoa(width)

	statement := fmt.Sprintf(`SELECT
    toString(toUnixTimestamp(toStartOfInterval(Timestamp, toIntervalSecond({bucket:UInt32})))) AS bucket,
    toString(count()) AS hits,
    toString(countIf(%[1]s IN ('error', 'fatal'))) AS errors,
    toString(countIf(%[1]s = 'warn')) AS warnings
FROM %[2]s.%[3]s
%[4]s
GROUP BY bucket
ORDER BY bucket
FORMAT JSONEachRow`,
		logLevelColumn, quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable), where)

	rows, err := c.selectionRows(ctx, statement, params)
	if err != nil {
		return LogHistogram{}, err
	}

	step := time.Duration(width) * time.Second
	count := int(end.Sub(start)/step) + 1
	if count > MaxHistogramBuckets {
		count = MaxHistogramBuckets
	}
	histogram := LogHistogram{
		Start:         start,
		End:           end,
		BucketSeconds: width,
		Buckets:       make([]LogBucket, count),
	}
	for i := range histogram.Buckets {
		histogram.Buckets[i].Start = start.Add(time.Duration(i) * step)
	}
	for _, row := range rows {
		seconds, err := strconv.ParseInt(row["bucket"], 10, 64)
		if err != nil {
			continue
		}
		i := int(time.Unix(seconds, 0).UTC().Sub(start) / step)
		if i < 0 || i >= len(histogram.Buckets) {
			continue
		}
		histogram.Buckets[i].Count = parseUint(row["hits"])
		histogram.Buckets[i].Errors = parseUint(row["errors"])
		histogram.Buckets[i].Warnings = parseUint(row["warnings"])
		histogram.Total += histogram.Buckets[i].Count
	}
	return histogram, nil
}

// logWindow is the span the histogram is drawn over: the selection's own
// bounds where it has them, and what the matching lines actually span where it
// does not.
func (c *Client) logWindow(ctx context.Context, selection LogSelection) (time.Time, time.Time, error) {
	start, end := selection.Since, selection.Until
	// "The last hour" is a start with no end, which is the common case and
	// needs no round trip: the window runs to now. Trailing buckets with
	// nothing in them are the truth about a store that is behind, not a gap to
	// be trimmed away.
	if !start.IsZero() && end.IsZero() {
		end = time.Now().UTC()
	}
	if !start.IsZero() && !end.IsZero() {
		return start.UTC(), end.UTC(), nil
	}

	where, params, err := selection.whereClause()
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	statement := fmt.Sprintf(`SELECT
    toString(toUnixTimestamp(min(Timestamp))) AS first,
    toString(toUnixTimestamp(max(Timestamp))) AS last
FROM %s.%s
%s
FORMAT JSONEachRow`,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable), where)

	rows, err := c.selectionRows(ctx, statement, params)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if len(rows) == 0 {
		return time.Time{}, time.Time{}, nil
	}
	if start.IsZero() {
		seconds, err := strconv.ParseInt(rows[0]["first"], 10, 64)
		if err != nil || seconds <= 0 {
			return time.Time{}, time.Time{}, nil
		}
		start = time.Unix(seconds, 0).UTC()
	}
	if end.IsZero() {
		seconds, err := strconv.ParseInt(rows[0]["last"], 10, 64)
		if err != nil || seconds <= 0 {
			return time.Time{}, time.Time{}, nil
		}
		// The bucket the newest line is in is a bucket, not a boundary.
		end = time.Unix(seconds, 0).UTC().Add(time.Second)
	}
	return start.UTC(), end.UTC(), nil
}

// bucketSeconds picks the narrowest width on the ladder that fits the span into
// the requested number of bars.
func bucketSeconds(span time.Duration, buckets int) int {
	wanted := int(span.Seconds()) / buckets
	for _, width := range bucketLadder {
		if width >= wanted {
			return width
		}
	}
	return bucketLadder[len(bucketLadder)-1]
}

// DefaultFacetFields are the columns worth offering before the user has said
// anything: whose the line is, what it came out of, and how bad it was.
var DefaultFacetFields = []string{"level", "source", "project", "environment", "container", "stream"}

// MaxFacetValues bounds one facet's values. A facet is a list to click, not a
// report; past a couple of dozen entries it is neither.
const MaxFacetValues = 20

// LogFacetQuery asks what values a selection's lines actually hold.
type LogFacetQuery struct {
	LogSelection
	// Fields names the columns to count. Anything that is not a column is an
	// attribute of the line, exactly as in the query language, so
	// `http.status` facets over `LogAttributes['http.status']`.
	Fields []string
	// Limit is the values per facet, most common first.
	Limit int
}

// LogFacetValue is one distinct value and how often the selection holds it.
type LogFacetValue struct {
	Value string `json:"value"`
	Count uint64 `json:"count"`
}

// LogFacet is one field's values. Values counts what was returned; Distinct
// counts what there was, so a truncated facet can say so.
type LogFacet struct {
	Field    string          `json:"field"`
	Values   []LogFacetValue `json:"values"`
	Distinct uint64          `json:"distinct"`
}

// LogFacets counts each field's distinct values over the whole selection.
//
// The counts are over the window, not over the page of lines that came back —
// which is the entire point of asking the store rather than counting in the
// browser. Every field is one subquery of a single UNION ALL, so a sidebar
// costs one round trip however many facets it shows.
func (c *Client) LogFacets(ctx context.Context, query LogFacetQuery) ([]LogFacet, error) {
	fields := query.Fields
	if len(fields) == 0 {
		fields = DefaultFacetFields
	}
	limit := query.Limit
	if limit < 1 || limit > MaxFacetValues {
		limit = MaxFacetValues
	}

	where, params, err := query.whereClause()
	if err != nil {
		return nil, err
	}
	params["facetLimit"] = strconv.Itoa(limit)

	// The facet expressions are resolved through the query language's own
	// resolver, so `service` means here what it means there, and a facet on
	// `http.status` reaches the structured field the same way a query does.
	// Its parameters carry their own prefix so they cannot collide with the
	// selection's.
	resolver := &logQueryParser{params: map[string]string{}, prefix: "facet"}
	selects := make([]string, 0, len(fields))
	for _, field := range fields {
		expression, _, err := resolver.columnExpression(field)
		if err != nil {
			return nil, err
		}
		selects = append(selects, fmt.Sprintf(`(SELECT
    {%s:String} AS facet,
    %s AS value,
    toString(count()) AS hits
FROM %s.%s
%s
GROUP BY value
HAVING value != ''
ORDER BY count() DESC, value ASC
LIMIT {facetLimit:UInt32})`,
			resolver.param(field), expression,
			quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable), where))
	}
	for name, value := range resolver.params {
		params[name] = value
	}

	rows, err := c.selectionRows(ctx, strings.Join(selects, "\nUNION ALL\n")+"\nFORMAT JSONEachRow", params)
	if err != nil {
		return nil, err
	}

	// UNION ALL does not promise an order across its branches, so the answer
	// is regrouped into the order the caller asked the fields in and each
	// facet is re-sorted.
	byField := map[string][]LogFacetValue{}
	for _, row := range rows {
		byField[row["facet"]] = append(byField[row["facet"]], LogFacetValue{
			Value: row["value"],
			Count: parseUint(row["hits"]),
		})
	}
	facets := make([]LogFacet, 0, len(fields))
	for _, field := range fields {
		// A field nothing in the window holds contributes no rows at all, and
		// a nil slice marshals to `null` rather than `[]` — which is a facet
		// the dashboard cannot iterate. An absent facet is an empty one.
		values := byField[field]
		if values == nil {
			values = []LogFacetValue{}
		}
		slices.SortStableFunc(values, func(a, b LogFacetValue) int {
			if a.Count != b.Count {
				return cmp.Compare(b.Count, a.Count)
			}
			return cmp.Compare(a.Value, b.Value)
		})
		facets = append(facets, LogFacet{
			Field:    field,
			Values:   values,
			Distinct: uint64(len(values)),
		})
	}
	return facets, nil
}

// Pattern extraction bounds. Normalising a message is a regular expression per
// line, which is the one thing here that is not a columnar scan, so it runs
// over a bounded slice of the newest lines rather than the whole window.
const (
	DefaultPatternScan = 20000
	MaxPatternScan     = 200000
	DefaultPatternRows = 20
	MaxPatternRows     = 200
)

// LogPatternQuery clusters a selection's lines into templates.
type LogPatternQuery struct {
	LogSelection
	// Limit is how many templates to return, commonest first.
	Limit int
	// Scan bounds how many of the newest matching lines are read. Patterns
	// are a shape, and the shape of the newest 20,000 lines is the shape.
	Scan int
}

// LogPattern is one template and what it stands for.
type LogPattern struct {
	Pattern   string    `json:"pattern"`
	Count     uint64    `json:"count"`
	Level     string    `json:"level,omitempty"`
	Sample    string    `json:"sample"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// patternNormalisers are applied in order, outermost last. Order matters: a
// timestamp has to be recognised before its parts are read as numbers, and an
// address before its dots are.
//
// The number rule deliberately has no closing `\b`, so that `200ms` becomes
// `<n>ms` rather than being left alone — RE2 does not backtrack, and a trailing
// boundary makes every number that touches a letter unmatchable.
var patternNormalisers = []struct{ pattern, replacement string }{
	{`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`, `<uuid>`},
	{`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?`, `<time>`},
	{`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?`, `<addr>`},
	{`\b(0x)?[0-9a-fA-F]{12,}\b`, `<hex>`},
	{`\b\d+(\.\d+)?`, `<n>`},
}

// LogPatterns collapses the selection's lines into templates.
//
// A spike is rarely 14,021 different things; it is one thing 14,021 times. The
// variable parts of a message — identifiers, addresses, durations, counts — are
// replaced with placeholders and what is left is grouped, so the page can say
// what is being said instead of showing the first screenful of it.
func (c *Client) LogPatterns(ctx context.Context, query LogPatternQuery) ([]LogPattern, error) {
	limit := query.Limit
	if limit < 1 {
		limit = DefaultPatternRows
	}
	if limit > MaxPatternRows {
		limit = MaxPatternRows
	}
	scan := query.Scan
	if scan < 1 {
		scan = DefaultPatternScan
	}
	if scan > MaxPatternScan {
		scan = MaxPatternScan
	}

	where, params, err := query.whereClause()
	if err != nil {
		return nil, err
	}
	params["limit"] = strconv.Itoa(limit)
	params["scan"] = strconv.Itoa(scan)

	normalised := logMessageColumn
	for _, normaliser := range patternNormalisers {
		normalised = fmt.Sprintf("replaceRegexpAll(%s, %s, %s)",
			normalised, quoteLiteral(normaliser.pattern), quoteLiteral(normaliser.replacement))
	}

	statement := fmt.Sprintf(`SELECT
    pattern,
    toString(count()) AS hits,
    any(level) AS level,
    any(message) AS sample,
    toString(toUnixTimestamp(min(ts))) AS first,
    toString(toUnixTimestamp(max(ts))) AS last
FROM (
    SELECT Timestamp AS ts, %s AS level, %s AS message, %s AS pattern
    FROM %s.%s
    %s
    ORDER BY Timestamp DESC
    LIMIT {scan:UInt32}
)
GROUP BY pattern
ORDER BY count() DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		logLevelColumn, logMessageColumn, normalised,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable), where)

	rows, err := c.selectionRows(ctx, statement, params)
	if err != nil {
		return nil, err
	}

	patterns := make([]LogPattern, 0, len(rows))
	for _, row := range rows {
		pattern := LogPattern{
			Pattern: row["pattern"],
			Count:   parseUint(row["hits"]),
			Level:   row["level"],
			Sample:  row["sample"],
		}
		if seconds, err := strconv.ParseInt(row["first"], 10, 64); err == nil {
			pattern.FirstSeen = time.Unix(seconds, 0).UTC()
		}
		if seconds, err := strconv.ParseInt(row["last"], 10, 64); err == nil {
			pattern.LastSeen = time.Unix(seconds, 0).UTC()
		}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}

// selectionRows runs an aggregate that carries caller-written query text and
// reads its JSONEachRow answer as strings. Everything an analytic selects is
// cast to String in the statement, so there is one decoding path and no
// surprises about how ClickHouse renders a UInt64 into JSON.
func (c *Client) selectionRows(ctx context.Context, statement string, params map[string]string) ([]map[string]string, error) {
	body, err := c.queryWithSettings(ctx, statement, params, readonlySettings)
	if err != nil {
		return nil, err
	}
	rows := []map[string]string{}
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable aggregate row: %w", err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseUint(value string) uint64 {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}
