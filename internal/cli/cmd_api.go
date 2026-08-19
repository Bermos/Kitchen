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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// `kitchen api` — the whole REST API, authenticated, with no command in the
// way.
//
// It is here for two reasons and they point the same way. The first is that
// the API is larger than this CLI and always will be: domains, claims,
// connections, the audit log, the platform screens, whatever lands next. The
// second is that a caller that is not a person should never reach the end of
// what the CLI models and stop — `kitchen schema` tells it this exists, and
// docs/API.md tells it what to send.
//
// So a route added to internal/api/policy.go is usable from the command line
// the day it lands. Whether it deserves a command of its own is then a
// judgement about how often somebody types it, rather than a question of
// whether the platform can be driven at all.

func newAPICommand(r *Runtime) *cobra.Command {
	var (
		data      string
		query     []string
		stream    bool
		showError bool
	)

	cmd := &cobra.Command{
		Use:   "api METHOD PATH",
		Short: "Call any endpoint of the platform's REST API",
		Long: strings.TrimSpace(`
Send an authenticated request to any endpoint of the platform's API and print
what comes back.

The path may be written with or without the /api/v1 prefix. The body is JSON:
--data takes it literally, @file reads a file, and - reads stdin.

  kitchen api GET /projects
  kitchen api POST /projects/shop/builds --data '{"sha":"abc123","branch":"main"}'
  kitchen api PATCH /environments/shop-production --data '{"release":"shop-rel-41"}'
  kitchen api GET /builds/shop-bld-abc-xk2p9/logs --stream

Every endpoint, its body and what it requires of the caller are documented in
docs/API.md. The exit status is this CLI's usual one — 4 for a refusal, 5 for
something that is not there — so a script can branch on the outcome without
parsing the body.`),
		Args: cobra.ExactArgs(2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			return callAPI(commandContext(cmd), r, args[0], args[1], data, query, stream, showError)
		}),
	}

	flags := cmd.Flags()
	flags.StringVarP(&data, "data", "d", "", "a JSON body: the JSON itself, @file, or - for stdin")
	flags.StringArrayVar(&query, "query", nil, "a query parameter as name=value, repeatable")
	flags.BoolVar(&stream, "stream", false,
		"follow the endpoint as Server-Sent Events, printing one JSON object per line")
	flags.BoolVar(&showError, "ignore-status", false,
		"print the body and exit 0 even when the platform refused, for a caller that wants to read the refusal")

	return describe(cmd, meta{
		Calls:  []string{"any endpoint under /api/v1 — see docs/API.md"},
		Output: output{Mode: outputDocument, Note: "the platform's own response body, unchanged. --stream makes it NDJSON"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"Read something no command covers yet", "kitchen api GET /domains --json"},
			{"Attach a custom domain",
				`kitchen api POST /domains --data '{"host":"shop.example.com","environment":"shop-production"}'`},
			{"Follow a build's logs as events", "kitchen api GET /builds/shop-bld-abc-xk2p9/logs --stream"},
		},
	})
}

func callAPI(
	parent context.Context,
	r *Runtime,
	method, path, data string,
	query []string,
	stream, ignoreStatus bool,
) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if !knownMethod(method) {
		return failf(codeUsage, "%q is not an HTTP method", method).
			withHint("one of GET, POST, PATCH, PUT, DELETE, HEAD")
	}
	path = apiPath(path)

	parameters, err := queryParameters(query)
	if err != nil {
		return err
	}
	body, err := requestBody(r, data)
	if err != nil {
		return err
	}

	ctx, cancel := r.context(parent)
	defer cancel()

	printer := r.printer()
	if stream {
		err := client.stream(ctx, "following "+path, strings.TrimPrefix(path, apiPrefix), parameters,
			func(payload []byte) error {
				return printer.event(json.RawMessage(payload), func(tui.Styles) string {
					return string(payload)
				})
			})
		if ctx.Err() != nil {
			return interrupted(ctx, "the stream is still open on the platform")
		}
		return err
	}

	status, answer, err := client.raw(ctx, method, path, parameters, body)
	if err != nil {
		return annotate(err, method+" "+path)
	}

	// The body is printed whatever the status: a refusal names the role it
	// wanted, and that sentence is the most useful thing the request produced.
	if err := printBody(printer, answer); err != nil {
		return err
	}
	if status >= 400 && !ignoreStatus {
		return fromStatus(status, answer).doing(method + " " + path)
	}
	return nil
}

// printBody writes what came back. Under --json it is passed through
// unchanged — the platform's bytes, not a re-encoding of them — and for a
// person it is indented, which is the one thing a terminal wants that a pipe
// does not.
func printBody(p *printer, answer []byte) error {
	if len(bytes.TrimSpace(answer)) == 0 {
		return nil
	}
	if p.json {
		_, err := p.out.Write(append(bytes.TrimRight(answer, "\n"), '\n'))
		return err
	}
	indented := &bytes.Buffer{}
	if err := json.Indent(indented, answer, "", "  "); err != nil {
		// Not JSON at all. Whatever it is, it is what the platform said.
		_, err := p.out.Write(append(bytes.TrimRight(answer, "\n"), '\n'))
		return err
	}
	_, err := p.out.Write(append(indented.Bytes(), '\n'))
	return err
}

// apiPath accepts the three ways somebody will write a path — with the
// prefix, without it, and without the leading slash — and answers the one the
// server serves.
func apiPath(path string) string {
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.HasPrefix(path, apiPrefix+"/") || path == apiPrefix {
		return path
	}
	return apiPrefix + path
}

func knownMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch,
		http.MethodPut, http.MethodDelete, http.MethodHead:
		return true
	default:
		return false
	}
}

// queryParameters reads the repeatable name=value flag.
func queryParameters(pairs []string) (url.Values, error) {
	parameters := url.Values{}
	for _, pair := range pairs {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return nil, failf(codeUsage, "--query %q is not name=value", pair)
		}
		parameters.Add(name, value)
	}
	return parameters, nil
}

// requestBody reads --data: the JSON itself, a file, or stdin. It is checked
// for being JSON here rather than being sent and refused, so a quoting mistake
// is a message about the quoting rather than a 400 about the platform.
func requestBody(r *Runtime, data string) ([]byte, error) {
	if data == "" {
		return nil, nil
	}

	var body []byte
	switch {
	case data == "-":
		read, err := io.ReadAll(io.LimitReader(r.Stdin, 32<<20))
		if err != nil {
			return nil, fail(codeUsage, "reading the body from stdin: "+err.Error())
		}
		body = read
	case strings.HasPrefix(data, "@"):
		read, err := os.ReadFile(strings.TrimPrefix(data, "@"))
		if err != nil {
			return nil, fail(codeUsage, "reading the body: "+err.Error())
		}
		body = read
	default:
		body = []byte(data)
	}

	if !json.Valid(body) {
		return nil, fail(codeUsage, "the body is not valid JSON").
			withHint("this API takes JSON on every write; --data @file and --data - avoid the shell's quoting")
	}
	return body, nil
}
