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
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// How the CLI fails, as a table.
//
// The exit code is the only thing a caller who is not reading the output can
// act on, so it is a contract rather than an implementation detail: `kitchen
// schema` publishes this table verbatim, and a script that branches on 4 will
// keep meaning "your account may not do that" for as long as the CLI exists.
//
// Every code here is also the `error.code` in the JSON body of a failure, so a
// caller reading stdout and a caller reading `$?` are told the same thing in
// the same words. failureCodes is the one place that pairs them.
const (
	exitOK              = 0
	exitFailed          = 1
	exitUsage           = 2
	exitUnauthenticated = 3
	exitForbidden       = 4
	exitNotFound        = 5
	exitConflict        = 6
	exitUnavailable     = 7
	exitUnreachable     = 8
	exitBuildFailed     = 9
	exitNotLinked       = 10
	exitTimedOut        = 11
	exitDeployFailed    = 12
	// exitInterrupted follows the shell's convention for SIGINT (128 + 2), so
	// a caller cannot mistake "somebody pressed ctrl-c" for a refusal.
	exitInterrupted = 130
)

// The failure codes, spelled once. A code names *why* something failed in a
// word a machine can switch on; the exit status is the same fact for a caller
// that only has `$?`.
const (
	codeFailed          = "failed"
	codeUsage           = "usage"
	codeUnauthenticated = "unauthenticated"
	codeForbidden       = "forbidden"
	codeNotFound        = "notFound"
	codeConflict        = "conflict"
	codeUnavailable     = "unavailable"
	codeUnreachable     = "unreachable"
	codeBuildFailed     = "buildFailed"
	codeNotLinked       = "notLinked"
	codeTimedOut        = "timedOut"
	codeDeployFailed    = "deployFailed"
	codeInterrupted     = "interrupted"
)

// exitCodeSpec is one row of the table above, with the sentence `kitchen
// schema` publishes it as.
type exitCodeSpec struct {
	Code    int    `json:"code"`
	Name    string `json:"name"`
	Meaning string `json:"meaning"`
}

// exitCodes is the published table. Order is by code, because that is how
// somebody reading a script's `case` statement will look for a row.
var exitCodes = []exitCodeSpec{
	{exitOK, "ok", "The command did what it was asked to do"},
	{exitFailed, codeFailed, "The command failed for a reason with no code of its own"},
	{exitUsage, codeUsage, "The command line itself is wrong: an unknown flag, a missing argument, a value that does not parse"},
	{exitUnauthenticated, codeUnauthenticated, "No usable credential, or the platform refused the one there was. `kitchen login` fixes it"},
	{exitForbidden, codeForbidden, "The account may not do this. The message names the role the operation wanted"},
	{exitNotFound, codeNotFound, "No such object — or one this account may not know exists, which the API answers identically on purpose"},
	{exitConflict, codeConflict, "Something else changed it first, it already exists, it already finished, or something still uses it"},
	{exitUnavailable, codeUnavailable, "A capability the endpoint needs is not installed on that platform"},
	{exitUnreachable, codeUnreachable, "The API could not be reached at all: DNS, TLS, a connection refused, a timeout on the wire"},
	{exitBuildFailed, codeBuildFailed, "A followed build ended Failed or Cancelled. The command worked; the build did not"},
	{exitNotLinked, codeNotLinked, "No project could be resolved. Pass --project, set KITCHEN_PROJECT, or run `kitchen link`"},
	{exitTimedOut, codeTimedOut, "A wait ran out before what it was waiting for happened. Nothing was undone"},
	{exitDeployFailed, codeDeployFailed,
		"A followed deploy ended Degraded: the build succeeded and the release did not take traffic. " +
			"What was serving before it still is"},
	{exitInterrupted, codeInterrupted, "Interrupted (SIGINT). Whatever was already started on the platform keeps running"},
}

// exitFor is the status a failure code exits with. An unknown code is a
// programming error rather than a state a caller can reach, and it exits 1
// rather than 0: a failure must never leave a zero status behind.
func exitFor(code string) int {
	for _, spec := range exitCodes {
		if spec.Name == code {
			return spec.Code
		}
	}
	return exitFailed
}

// failure is every way this CLI stops early, in the shape it is published in.
//
// Message is the sentence — the API's own where the API refused, since it is
// written to be read by whoever sent the request, and the CLI's where the CLI
// refused. Hint is what would fix it, and is the half that makes a failure
// worth reading: the rule the chart's guards follow (say what is wrong *and*
// what would fix it) is the rule here.
type failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	// Status is the HTTP status the API answered with, when the failure came
	// from it. Zero for a failure the CLI decided by itself.
	Status int `json:"status,omitempty"`
	// Doing names the operation, so a failure reads as a sentence without
	// every call site writing one: "starting a build: ...".
	Doing string `json:"doing,omitempty"`
}

func (f *failure) Error() string {
	if f.Doing == "" {
		return f.Message
	}
	return f.Doing + ": " + f.Message
}

// exitCode is the status this failure ends the process with.
func (f *failure) exitCode() int { return exitFor(f.Code) }

// asFailure reads a failure out of an error chain, describing anything else as
// a plain one. Every path out of a command therefore has a code and an exit
// status, including the ones that return an error from somewhere else.
func asFailure(err error) *failure {
	var f *failure
	if errors.As(err, &f) {
		return f
	}
	return &failure{Code: codeFailed, Message: err.Error()}
}

func fail(code, message string) *failure {
	return &failure{Code: code, Message: message}
}

func failf(code, format string, args ...any) *failure {
	return &failure{Code: code, Message: fmt.Sprintf(format, args...)}
}

// withHint attaches what would fix it.
func (f *failure) withHint(hint string) *failure {
	f.Hint = hint
	return f
}

// doing names the operation a failure happened during. It is set by the client
// rather than by each caller, so "starting a build" appears once.
func (f *failure) doing(what string) *failure {
	f.Doing = what
	return f
}

// errorBody is the API's own error shape: {"error": "..."}.
type errorBody struct {
	Error string `json:"error"`
}

// fromStatus turns an API response into a failure, keeping the API's sentence
// and adding the code and the exit status this CLI publishes for it.
//
// The mapping is the one docs/API.md documents, and it is deliberately narrow:
// a status the API does not use answers `failed`, rather than being guessed at
// a class boundary.
func fromStatus(status int, body []byte) *failure {
	message := strings.TrimSpace(string(body))
	answer := errorBody{}
	if err := json.Unmarshal(body, &answer); err == nil && answer.Error != "" {
		message = answer.Error
	}
	if message == "" {
		message = http.StatusText(status)
	}

	f := &failure{Message: message, Status: status}
	switch status {
	case http.StatusUnauthorized:
		f.Code = codeUnauthenticated
		f.Hint = "the platform refused the credential: run `kitchen login` again, or check KITCHEN_TOKEN"
	case http.StatusForbidden:
		f.Code = codeForbidden
	case http.StatusNotFound:
		f.Code = codeNotFound
		f.Hint = "an object somebody else's project owns is answered exactly like one that is not there, " +
			"so check the name and check whether this account holds a role on that project"
	case http.StatusConflict:
		f.Code = codeConflict
	case http.StatusServiceUnavailable:
		f.Code = codeUnavailable
	default:
		f.Code = codeFailed
	}
	return f
}
