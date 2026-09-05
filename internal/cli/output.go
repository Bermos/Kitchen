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

package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// noValue is what a text table prints where a field is absent. An em dash
// rather than an empty cell, so that "nothing here" reads as a deliberate
// answer rather than as a column that failed to render — and one constant
// rather than one per command, so the tables agree with each other.
const noValue = "—"

// The output contract, which is the whole reason this CLI is worth a machine's
// time. It has exactly two modes and they never mix:
//
//   - **--json.** stdout carries JSON and nothing else. A command that answers
//     with one thing writes one document; a command that follows something
//     writes one JSON object per line (NDJSON), each with a `type`, ending in
//     a `result` or an `error` event. A failure is a single
//     `{"error": {...}}` document, whatever the mode, so a caller can read the
//     reason without knowing which command it ran. Progress, warnings and
//     anything else a person would want go to stderr as NDJSON, where they
//     cannot corrupt the answer.
//   - **text.** Whatever reads best for a person: styled when stdout is a
//     terminal, the same words unstyled when it is not, so `kitchen logs |
//     grep` is not full of escape sequences.
//
// Nothing is written to stdout that is not the answer. That is what makes
// `kitchen … --json | jq` safe in a script that has not read this file.

// printer writes a command's answer.
type printer struct {
	out    io.Writer
	err    io.Writer
	json   bool
	styles tui.Styles
}

// document writes a command's whole answer: one JSON document, or whatever
// render draws for a person. Exactly one of the two runs, so a command cannot
// accidentally write its answer twice.
func (p *printer) document(value any, render func(tui.Styles) string) error {
	if p.json {
		return p.writeJSON(p.out, value)
	}
	if render == nil {
		return nil
	}
	_, err := io.WriteString(p.out, render(p.styles))
	return err
}

// event writes one line of a stream: an NDJSON object in JSON mode, a line of
// text otherwise. The text is a function so a stream that costs something to
// format does not format it into a pipe that is about to discard it.
func (p *printer) event(value any, render func(tui.Styles) string) error {
	if p.json {
		return p.writeJSON(p.out, value)
	}
	if render == nil {
		return nil
	}
	_, err := fmt.Fprintln(p.out, render(p.styles))
	return err
}

// note is for a person: progress, a warning, anything that is not the answer.
// It goes to stderr in both modes — as a `note` event under --json, so a
// caller that reads both streams as JSON is not handed a bare sentence.
func (p *printer) note(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if p.json {
		_ = p.writeJSON(p.err, map[string]string{"type": "note", "message": message})
		return
	}
	_, _ = fmt.Fprintln(p.err, p.styles.Subtle.Render(message))
}

// warn is a note that something is off but the command is carrying on — a
// dirty working tree, a commit that is not on the remote yet.
func (p *printer) warn(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if p.json {
		_ = p.writeJSON(p.err, map[string]string{"type": "warning", "message": message})
		return
	}
	_, _ = fmt.Fprintln(p.err, p.styles.Warn.Render("! ")+message)
}

// failure writes the one shape every failing command ends with. It goes to
// stdout under --json, because it *is* the answer to the request and a caller
// reading one stream should not have to read two to find out that nothing
// came; in text mode it goes to stderr, where a person's shell already sends
// errors.
func (p *printer) failure(f *failure) {
	if p.json {
		_ = p.writeJSON(p.out, map[string]*failure{"error": f})
		return
	}
	// A refusal's message is often the platform's own words quoted back —
	// an admission webhook's, a provider's, an application's — so it is text
	// this process did not write and goes through safeText like every other
	// relayed line. The hint is always ours and needs nothing.
	_, _ = fmt.Fprintln(p.err, p.styles.Bad.Render("Error:")+" "+safeText(f.Error()))
	if f.Hint != "" {
		_, _ = fmt.Fprintln(p.err, p.styles.Subtle.Render("  "+f.Hint))
	}
}

// writeJSON writes one compact document per line. Compact rather than indented
// so that a stream is NDJSON — one object per line, which is what makes
// `while read` and `jq -c` work on it — and a single document is still a
// single line, which nothing minds.
func (p *printer) writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
