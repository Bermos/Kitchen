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
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// An application's log line is text an untrusted party wrote, and an operator
// reads it in a terminal that acts on what is in it. These pin the two halves
// of the answer: a person's rendering is stripped, and --json is not.

// The line one would print to clear somebody's screen, retitle their window
// and paint the rest of the tail green — with a C1 control substituted for
// `ESC [` in the last one, which is the form a filter that looks only for
// U+001B lets through.
const nastyLine = "deploy \x1b[2Jok \x1b]0;pwned\x07 and \u009b32mgreen\u009b0m\x00done"

func TestSafeTextStripsWhatATerminalWouldActOn(t *testing.T) {
	got := safeText(nastyLine)
	for _, forbidden := range []string{"\x1b", "\u009b", "\x07", "\x00"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("%q survived: %q", forbidden, got)
		}
	}
	if want := "deploy [2Jok ]0;pwned and 32mgreen0mdone"; got != want {
		t.Errorf("the words must survive with the instructions gone\n got %q\nwant %q", got, want)
	}
}

// Newline and tab are the two controls that carry meaning in a log line: a
// multi-line message is still one line and an aligned one is still aligned.
func TestSafeTextKeepsNewlinesAndTabs(t *testing.T) {
	line := "first\n\tsecond — ünïcode ✓"
	if got := safeText(line); got != line {
		t.Errorf("ordinary text must pass through untouched\n got %q\nwant %q", got, line)
	}
}

// The rendered log line is what `kitchen logs` and the followed half of
// `kitchen deploy` both print, so stripping it there covers both.
func TestRenderLogLineStripsTheServersText(t *testing.T) {
	line := logLine{
		Timestamp: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
		Container: "web\x1b[2J",
		Level:     "info\x1b[31m",
		Message:   nastyLine,
	}
	got := renderLogLine(tui.New(false), line, true)
	if strings.ContainsAny(got, "\x1b\u009b\x07\x00") {
		t.Fatalf("a rendered log line still carries a control sequence: %q", got)
	}
	if !strings.Contains(got, "done") {
		t.Fatalf("the message must still be readable: %q", got)
	}
}

// --json is the answer byte for byte: a caller that asked for JSON asked for
// what the platform said, and jq does not act on an escape sequence.
func TestJSONOutputIsNotStripped(t *testing.T) {
	out := &bytes.Buffer{}
	p := &printer{out: out, err: &bytes.Buffer{}, json: true, styles: tui.New(false)}
	if err := p.event(logLine{Message: nastyLine}, func(tui.Styles) string { return "unused" }); err != nil {
		t.Fatal(err)
	}
	// Round-tripped: the document a caller reads back carries the message the
	// platform sent, rune for rune.
	decoded := logLine{}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Message != nastyLine {
		t.Fatalf("--json must relay the platform's message unchanged\n got %q\nwant %q", decoded.Message, nastyLine)
	}
}

// `kitchen api` reaches every endpoint there is, the ones that answer with an
// application's own lines included. A terminal gets the stripped body; a pipe
// gets the platform's bytes, so redirecting the command to a file gives the
// same file whichever way it ran.
func TestAPIBodyIsStrippedOnlyForATerminal(t *testing.T) {
	body := []byte(`{"message":"` + "\x1b[2Jgone" + `"}`)

	terminal := &bytes.Buffer{}
	p := &printer{out: terminal, err: &bytes.Buffer{}, styles: tui.New(false)}
	if err := printBody(p, body, true); err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsAny(terminal.Bytes(), "\x1b") {
		t.Errorf("a terminal must not be handed an escape: %q", terminal.String())
	}

	piped := &bytes.Buffer{}
	q := &printer{out: piped, err: &bytes.Buffer{}, styles: tui.New(false)}
	if err := printBody(q, body, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(piped.Bytes(), []byte("\x1b")) {
		t.Errorf("a pipe gets the platform's bytes: %q", piped.String())
	}
}

// The single-byte form of `ESC [` arrives as a byte that is not valid UTF-8
// when nothing between here and the application re-encoded it. It must not
// reach the terminal either.
func TestSafeTextDropsBytesThatAreNotUTF8(t *testing.T) {
	got := safeText("green \x9b32m and on")
	if strings.ContainsRune(got, 0x9b) || strings.ContainsRune(got, 0xfffd) {
		t.Fatalf("a raw C1 introducer survived: %q", got)
	}
	if !strings.Contains(got, "and on") {
		t.Fatalf("the words must survive: %q", got)
	}
}

// A refusal's message is frequently the platform's own words quoted back, so
// the one line every failing command ends with is relayed text too. It goes to
// stderr, which is a terminal as much as stdout is.
func TestFailureMessageIsStripped(t *testing.T) {
	errOut := &bytes.Buffer{}
	p := &printer{out: &bytes.Buffer{}, err: errOut, styles: tui.New(false)}
	p.failure(&failure{Message: "refused: " + nastyLine})
	if bytes.ContainsAny(errOut.Bytes(), "\x1b\x00") {
		t.Fatalf("a refusal still carries a control sequence: %q", errOut.String())
	}
	if !bytes.Contains(errOut.Bytes(), []byte("refused")) {
		t.Fatalf("the words must survive: %q", errOut.String())
	}
}
