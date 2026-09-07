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
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// What people have done about the conditions the catalogue found.
//
// Like the transitions beside it, this package knows nothing about the
// catalogue: a mitigation is a row here — a kind, a delivery it is about, who
// did it and why — and what an acknowledgement *does* to a tier is
// internal/signals'. The arrow stays one way, which is what lets the API that
// writes these and the loop that reads them share a store.

// SignalMitigation is one record: what was done, to which delivery, by whom.
type SignalMitigation struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`

	// Fingerprint and Audience are the delivery. A condition delivered to
	// two audiences is acknowledged and silenced separately, which is the
	// whole reason neither half of the key is optional.
	Fingerprint string `json:"fingerprint"`
	Audience    string `json:"audience"`

	// Project is what the condition is about, where it is about one. It is
	// duplicated out of the transition so that "everything anybody did about
	// this project" is one read rather than a join.
	Project string `json:"project,omitempty"`

	// Actor is who did it, as the API knew them.
	Actor string `json:"actor"`
	// Reason is why. A silence must carry one; an acknowledgement need not.
	Reason string `json:"reason,omitempty"`
	// Until is when a silence expires, and the zero instant for everything
	// else.
	Until time.Time `json:"until,omitempty"`
	// Source is `explicit` for somebody pressing the button, and `action`
	// for the acknowledgement a resolving action records on their behalf.
	Source string `json:"source,omitempty"`
}

// InsertSignalMitigation records one. It is one row at a time on purpose: each
// one is a person's decision made at an instant, so there is no batch to
// amortise and no round in which several arrive together.
func (c *Client) InsertSignalMitigation(ctx context.Context, mitigation SignalMitigation) error {
	at := mitigation.At
	if at.IsZero() {
		at = time.Now()
	}
	// A record that is not a silence has no expiry, and the column is not
	// nullable: the epoch is what "no expiry" reads as, and Silenced never
	// asks the question of anything but a silence.
	until := mitigation.Until
	if until.IsZero() {
		until = time.Unix(0, 0).UTC()
	}
	row, err := json.Marshal(map[string]any{
		"timestamp":   at.UTC().Format(storeTimestampLayout),
		"kind":        mitigation.Kind,
		"fingerprint": mitigation.Fingerprint,
		"audience":    mitigation.Audience,
		"project":     mitigation.Project,
		"actor":       mitigation.Actor,
		"reason":      mitigation.Reason,
		"until":       until.UTC().Format(storeTimestampLayout),
		"source":      mitigation.Source,
	})
	if err != nil {
		return err
	}
	return c.Exec(ctx, fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow\n%s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(SignalMitigationsTable), string(row)))
}

// SignalMitigations is every record the table holds, newest last.
//
// The whole table rather than a filtered slice of it, because the fold that
// turns records into standing state is over all of them and the table is small
// by construction: one row per decision a person made, under the signal
// retention. A store that ever holds enough of these to be worth narrowing has
// an alerting problem no query can fix.
func (c *Client) SignalMitigations(ctx context.Context) ([]SignalMitigation, error) {
	statement := fmt.Sprintf(`SELECT
    formatDateTime(timestamp, '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC') AS %s,
    kind,
    fingerprint,
    audience,
    project,
    actor,
    reason,
    formatDateTime(until, '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC') AS until_ts,
    source
FROM %s.%s
ORDER BY timestamp
FORMAT JSONEachRow`,
		timestampAlias, quoteIdentifier(c.cfg.Database), quoteIdentifier(SignalMitigationsTable))

	body, err := c.Query(ctx, statement)
	if err != nil {
		return nil, err
	}
	return parseSignalMitigations(body)
}

// signalMitigationRow is the wire shape. The two timestamps come back as
// strings under aliases, for the reason timestampAlias gives.
type signalMitigationRow struct {
	At          string `json:"ts"`
	Kind        string `json:"kind"`
	Fingerprint string `json:"fingerprint"`
	Audience    string `json:"audience"`
	Project     string `json:"project"`
	Actor       string `json:"actor"`
	Reason      string `json:"reason"`
	Until       string `json:"until_ts"`
	Source      string `json:"source"`
}

func parseSignalMitigations(body string) ([]SignalMitigation, error) {
	mitigations := make([]SignalMitigation, 0, 8)
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := signalMitigationRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable signal mitigation row: %w", err)
		}
		at, err := time.Parse(otelTimestampLayout, row.At)
		if err != nil {
			return nil, fmt.Errorf("unreadable signal mitigation timestamp %q: %w", row.At, err)
		}
		mitigation := SignalMitigation{
			At:          at,
			Kind:        row.Kind,
			Fingerprint: row.Fingerprint,
			Audience:    row.Audience,
			Project:     row.Project,
			Actor:       row.Actor,
			Reason:      row.Reason,
			Source:      row.Source,
		}
		// The epoch is how a record with no expiry was written; reading it
		// back as one would make every acknowledgement look like a silence
		// that ended in 1970.
		if until, err := time.Parse(otelTimestampLayout, row.Until); err == nil && until.Unix() > 0 {
			mitigation.Until = until
		}
		mitigations = append(mitigations, mitigation)
	}
	return mitigations, nil
}
