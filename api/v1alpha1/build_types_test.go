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

package v1alpha1

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitCommitMessage(t *testing.T) {
	cases := []struct {
		name    string
		message string
		subject string
		body    string
	}{
		{
			name:    "a subject alone, which is most commits",
			message: "feat(api): add the route",
			subject: "feat(api): add the route",
		},
		{
			name:    "the body under it, blank line and all",
			message: "feat(api): add the route\n\nWhy it is shaped this way.\n\nCo-Authored-By: somebody\n",
			subject: "feat(api): add the route",
			body:    "Why it is shaped this way.\n\nCo-Authored-By: somebody",
		},
		{
			name:    "the shape of the body is kept, only what surrounds it goes",
			message: "fix: it\n\n  - one\n  - two\n",
			subject: "fix: it",
			body:    "  - one\n  - two",
		},
		{
			name:    "carriage returns are the provider's, not the reader's",
			message: "docs: say so\r\n\r\nA body.\r\n",
			subject: "docs: say so",
			body:    "A body.",
		},
		{name: "nothing at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject, body := SplitCommitMessage(tc.message)
			if subject != tc.subject {
				t.Errorf("subject: got %q, want %q", subject, tc.subject)
			}
			if body != tc.body {
				t.Errorf("body: got %q, want %q", body, tc.body)
			}
			if got := CommitSubject(tc.message); got != tc.subject {
				t.Errorf("CommitSubject: got %q, want %q", got, tc.subject)
			}
			if got := CommitBody(tc.message); got != tc.body {
				t.Errorf("CommitBody: got %q, want %q", got, tc.body)
			}
		})
	}
}

// A generated commit can carry a diff in its body, and a Build's spec is not
// where that belongs. What is kept has to stay valid UTF-8: the object it goes
// into is serialised as JSON, and half a rune is not a rune.
func TestSplitCommitMessageCutsALongBody(t *testing.T) {
	_, body := SplitCommitMessage("chore: regenerate\n\n" + strings.Repeat("é", CommitBodyLimit))
	if len(body) > CommitBodyLimit+len("…") {
		t.Errorf("body is %d bytes, over the %d-byte limit", len(body), CommitBodyLimit)
	}
	if !strings.HasSuffix(body, "…") {
		t.Errorf("a cut body says it was cut: %q", body[max(0, len(body)-8):])
	}
	if !utf8.ValidString(body) {
		t.Error("the cut left half a rune behind")
	}
}
