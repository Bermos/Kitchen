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
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The Server-Sent Events decoder is the one piece of wire format the CLI
// implements itself, and every followed build and tailed log goes through it.
func TestReadEventsDecodesTheStream(t *testing.T) {
	stream := strings.Join([]string{
		": keepalive",
		"",
		"data: {\"message\":\"one\"}",
		"",
		"data: {\"message\":",
		"data: \"two\"}",
		"",
		": keepalive",
		"",
		"data: {\"message\":\"three\"}",
		"",
	}, "\n")

	var messages []string
	err := readEvents(strings.NewReader(stream), func(payload []byte) error {
		line := logLine{}
		if err := json.Unmarshal(payload, &line); err != nil {
			return err
		}
		messages = append(messages, line.Message)
		return nil
	})
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if strings.Join(messages, ",") != "one,two,three" {
		t.Fatalf("unexpected messages: %v", messages)
	}
}

// An `event: error` is how the API reports a failure once the response has
// already started, so it has to become a failure here rather than a row.
func TestReadEventsTurnsAnErrorEventIntoAFailure(t *testing.T) {
	stream := "data: {\"message\":\"one\"}\n\nevent: error\ndata: {\"error\":\"the store went away\"}\n\n"

	rows := 0
	err := readEvents(strings.NewReader(stream), func([]byte) error {
		rows++
		return nil
	})
	if err == nil {
		t.Fatal("an error event was read as a row")
	}
	if rows != 1 {
		t.Fatalf("the rows before it were lost: %d", rows)
	}
	if failure := asFailure(err); !strings.Contains(failure.Message, "the store went away") {
		t.Fatalf("the platform's sentence was lost: %q", failure.Message)
	}
}

// The API URL is the first thing anybody types and the easiest to get wrong,
// so the CLI accepts every reasonable spelling of it.
func TestNormalizeAPIAcceptsWhatSomebodyWillType(t *testing.T) {
	for _, testCase := range []struct{ typed, wanted string }{
		{"https://kitchen.example.com", "https://kitchen.example.com"},
		{"https://kitchen.example.com/", "https://kitchen.example.com"},
		{"kitchen.example.com", "https://kitchen.example.com"},
		{"https://kitchen.example.com/api/v1", "https://kitchen.example.com"},
		{"https://kitchen.example.com/api/v1/", "https://kitchen.example.com"},
		{"  https://kitchen.example.com  ", "https://kitchen.example.com"},
		{"http://localhost:8080", "http://localhost:8080"},
	} {
		if got := normalizeAPI(testCase.typed); got != testCase.wanted {
			t.Errorf("%q became %q, wanted %q", testCase.typed, got, testCase.wanted)
		}
	}
}

func TestAPIPathAcceptsBothSpellings(t *testing.T) {
	for _, typed := range []string{"/projects", "projects", "/api/v1/projects"} {
		if got := apiPath(typed); got != "/api/v1/projects" {
			t.Errorf("%q became %q", typed, got)
		}
	}
}

// The merge is what makes a one-variable change possible against a route that
// replaces the whole list, so its rules are worth stating twice.
func TestMergeEnvKeepsWhatItWasNotAskedToChange(t *testing.T) {
	value := "debug"
	existing := []envVar{
		{Name: "API_KEY", Set: true},
		{Name: "DATABASE_URL", FromClaim: &keyRef{Name: "shop-db", Key: "url"}},
		{Name: "LOG_LEVEL", Set: true},
	}
	merged := mergeEnv(existing, map[string]*envVarWrite{
		"LOG_LEVEL": {Name: "LOG_LEVEL", Value: &value},
		"NEW_ONE":   {Name: "NEW_ONE", Value: &value},
	})

	if len(merged) != 4 {
		t.Fatalf("wanted four variables, got %d: %+v", len(merged), merged)
	}
	byName := map[string]envVarWrite{}
	for _, variable := range merged {
		byName[variable.Name] = variable
	}
	if byName["API_KEY"].Value != nil {
		t.Error("an untouched variable was sent with a value, which the CLI has never read")
	}
	if byName["DATABASE_URL"].FromClaim == nil {
		t.Error("an untouched reference was dropped: a reference is what the variable is, not a value it holds")
	}
	if got := byName["LOG_LEVEL"].Value; got == nil || *got != "debug" {
		t.Errorf("the changed variable: %+v", byName["LOG_LEVEL"])
	}
	// The order matters to nobody but a reader; what matters is that a new
	// variable arrives at all.
	if _, ok := byName["NEW_ONE"]; !ok {
		t.Error("a variable that did not exist was not added")
	}
}

// An empty value is a clear rather than a keep, which is the one place the
// pointer earns its keep.
func TestAnEmptyValueClearsAVariable(t *testing.T) {
	empty := ""
	merged := mergeEnv([]envVar{{Name: "LOG_LEVEL", Set: true}},
		map[string]*envVarWrite{"LOG_LEVEL": {Name: "LOG_LEVEL", Value: &empty}})

	if len(merged) != 1 || merged[0].Value == nil || *merged[0].Value != "" {
		t.Fatalf("unexpected merge: %+v", merged)
	}
	body, err := json.Marshal(merged[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"value":""`) {
		t.Fatalf("the empty value did not reach the wire: %s", body)
	}
}

func TestInstantReadsBothFormsOfAMoment(t *testing.T) {
	stamped, err := instant("2026-08-19T09:00:00Z")
	if err != nil || stamped != "2026-08-19T09:00:00Z" {
		t.Fatalf("timestamp: %q, %v", stamped, err)
	}

	ago, err := instant("15m")
	if err != nil {
		t.Fatalf("duration: %v", err)
	}
	at, err := time.Parse(time.RFC3339, ago)
	if err != nil {
		t.Fatalf("a duration did not become a timestamp: %q", ago)
	}
	if elapsed := time.Since(at); elapsed < 14*time.Minute || elapsed > 16*time.Minute {
		t.Fatalf("15m became %v ago", elapsed)
	}

	if empty, err := instant(""); err != nil || empty != "" {
		t.Fatalf("nothing should stay nothing: %q, %v", empty, err)
	}
	if _, err := instant("soon"); err == nil {
		t.Fatal("a word that is neither should be refused")
	}
}

// The token's expiry is read to decide when to exchange again, and a token it
// cannot read has to look expired rather than be used until it is refused.
func TestTokenExpiry(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1786000000}`))
	at := tokenExpiry("header." + claims + ".signature")
	if at == nil || at.Unix() != 1786000000 {
		t.Fatalf("unexpected expiry: %v", at)
	}

	for _, unreadable := range []string{"", "not-a-token", "a.b", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".c"} {
		if got := tokenExpiry(unreadable); got != nil {
			t.Errorf("%q read as expiring at %v", unreadable, got)
		}
	}
	if !expiring(nil) {
		t.Error("a token with no known expiry has to be treated as expired")
	}
	soon := time.Now().Add(30 * time.Second)
	if !expiring(&soon) {
		t.Error("a token expiring inside the request it is about to be used on is expired")
	}
	later := time.Now().Add(time.Hour)
	if expiring(&later) {
		t.Error("a token good for an hour was thrown away")
	}
}

// A failure always has a code and a non-zero status, however it was made.
func TestEveryFailureCarriesACode(t *testing.T) {
	plain := asFailure(errStub{})
	if plain.Code != codeFailed || plain.exitCode() == exitOK {
		t.Fatalf("an ordinary error became %+v", plain)
	}

	fromAPI := fromStatus(403, []byte(`{"error":"you have viewer on shop; redeploying needs developer"}`))
	if fromAPI.Code != codeForbidden || fromAPI.Status != 403 {
		t.Fatalf("unexpected failure: %+v", fromAPI)
	}
	if !strings.Contains(fromAPI.Message, "needs developer") {
		t.Fatalf("the API's sentence was lost: %q", fromAPI.Message)
	}

	// A body that is not the API's shape still has to say something.
	fromProxy := fromStatus(502, []byte("<html>bad gateway</html>"))
	if fromProxy.Message == "" || fromProxy.Code != codeFailed {
		t.Fatalf("unexpected failure: %+v", fromProxy)
	}

	// The operation is named once, by the client, and never overwritten.
	annotated := asFailure(annotate(fail(codeConflict, "it already finished").doing("first"), "second"))
	if annotated.Doing != "first" {
		t.Fatalf("the operation was renamed: %+v", annotated)
	}
}

type errStub struct{}

func (errStub) Error() string { return "something went wrong" }

// A commit that builds several images builds each in its own Job, and their
// output is one tail. The line says which of them wrote it; every other line
// on the platform is unchanged by that.
func TestLogLineNamesTheWorkloadThatWroteIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		line logLine
		want string
	}{
		{
			name: "a workload's own build Job",
			line: logLine{Build: "shop-bld-abc", Run: "shop-bld-abc-api"},
			want: "api",
		},
		{
			name: "the web process's, which is the build's own Job",
			line: logLine{Build: "shop-bld-abc", Run: "shop-bld-abc"},
			want: "",
		},
		{
			name: "a build of one image, whose lines carry the Job and nothing to tell apart",
			line: logLine{Build: "shop-bld-abc", Run: "shop-bld-abc", Container: "buildkit"},
			want: "",
		},
		{
			name: "one firing of a scheduled job, which is not a build at all",
			line: logLine{Run: "shop-production-nightly-report-29387520", Container: "app"},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.line.workload(); got != tc.want {
				t.Errorf("workload() = %q, want %q", got, tc.want)
			}
		})
	}
}
