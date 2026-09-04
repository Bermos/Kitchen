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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// Runtime is everything a command is allowed to touch outside itself: the
// three streams, the environment, the working directory, and whether there is
// a person on the other end.
//
// It exists so that no command reads os.Stdout, os.Getenv or the current
// directory directly. That is what makes the whole CLI testable without a
// terminal, a home directory or a network — and it is the same discipline that
// makes it *drivable*: a surface with no hidden inputs is one an agent can run
// with confidence that the answer depends on the arguments alone.
type Runtime struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Getenv reads the environment. Never os.Getenv directly.
	Getenv func(string) string
	// WorkingDir is where the link file is looked for and where git is asked
	// about the commit.
	WorkingDir string
	// Terminal reports that stdout is a terminal: the one thing that decides
	// whether the CLI paints anything or starts a Bubble Tea program.
	Terminal bool
	// StdinTerminal reports that stdin is a terminal, and so whether there is
	// anybody who could answer a prompt.
	StdinTerminal bool

	// The global flags, filled in by the root command before anything runs.
	apiFlag     string
	projectFlag string
	jsonOut     bool
	plain       bool
	noInput     bool
	timeout     time.Duration

	// Resolved once, on first use.
	out       *printer
	stored    *credentials
	storedDir string
	base      string
	http      *http.Client
}

// printer is the output contract, built from the flags and the terminal.
//
// Colour is on only when a person is watching *and* the answer is not JSON:
// escape sequences in a pipe are something the reader has to strip, and a
// document that has been painted is not JSON any more.
func (r *Runtime) printer() *printer {
	if r.out == nil {
		r.out = &printer{
			out:    r.Stdout,
			err:    r.Stderr,
			json:   r.jsonOut,
			styles: tui.New(r.Terminal && !r.jsonOut && !r.plain),
		}
	}
	return r.out
}

// interactive reports whether a command may take over the terminal — for a
// Bubble Tea program, or to ask a question. Everything that is not obviously a
// person watching answers false: piped output, --json, --plain, --no-input, or
// a terminal that cannot be read from.
func (r *Runtime) interactive() bool {
	return r.Terminal && r.StdinTerminal && !r.jsonOut && !r.plain && !r.noInput
}

// env reads one variable, trimmed. A variable set to whitespace is the same as
// one that is not set: a shell exporting an empty value is far more often an
// accident than an instruction.
func (r *Runtime) env(name string) string {
	if r.Getenv == nil {
		return ""
	}
	return strings.TrimSpace(r.Getenv(name))
}

// credentials reads the credential file once per process.
func (r *Runtime) credentials() (*credentials, string, error) {
	if r.stored != nil {
		return r.stored, r.storedDir, nil
	}
	dir, err := configHome(r.Getenv)
	if err != nil {
		return nil, "", err
	}
	stored, err := loadCredentials(dir)
	if err != nil {
		return nil, "", err
	}
	r.stored, r.storedDir = stored, dir
	return stored, dir, nil
}

// link is what the working directory says, or nil.
func (r *Runtime) link() (*link, string, error) {
	return findLink(r.WorkingDir)
}

// apiURL resolves which installation this command is about, in the order a
// person would expect to be able to override things: the flag, then the
// environment, then the working directory's link, then the machine's current
// installation.
//
// Every one of the four is spelled out in the failure, because "which
// platform" is the one question a caller with no context cannot guess at.
//
// The link file is the one of the four that anybody who can commit to the
// repository writes, so it only ever *chooses* an installation — see
// allowLinked.
func (r *Runtime) apiURL() (string, error) {
	if r.base != "" {
		return r.base, nil
	}
	candidates := []string{r.apiFlag, r.env("KITCHEN_API")}
	for _, candidate := range candidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			r.base = normalizeAPI(candidate)
			return r.base, nil
		}
	}
	if linked, dir, err := r.link(); err != nil {
		return "", err
	} else if linked != nil && linked.API != "" {
		base := normalizeAPI(linked.API)
		if err := r.allowLinked(base, dir); err != nil {
			return "", err
		}
		r.base = base
		return r.base, nil
	}
	stored, _, err := r.credentials()
	if err != nil {
		return "", err
	}
	if stored.Current != "" {
		r.base = stored.Current
		return r.base, nil
	}
	return "", fail(codeUsage, "no Kitchen installation to talk to").
		withHint("pass --api https://kitchen.<your-domain>, set KITCHEN_API, " +
			"run `kitchen login --api …`, or run `kitchen link` in this directory")
}

// allowLinked decides whether the link file may point this command at base.
//
// `.kitchen/project.json` is committed — that is the whole reason it exists —
// which makes it the one input to this CLI that anybody who can push to the
// repository writes, including a fork's pull request. A file that could name
// any host would therefore choose where a credential goes: the API key would
// be offered as `x-api-key` to whatever issuer that host's /config.json names,
// and KITCHEN_TOKEN would travel to it as a bearer. So the link file may
// **choose among the installations this machine already knows** — the ones
// `kitchen login` stored — and may never introduce a new one.
//
// A host that is not one of them is refused while there is a credential in the
// environment to lose, and offered to a person who is there to look at it.
// Which installation this is remains something the caller can always say
// outright, with --api or KITCHEN_API, and that is what CI should do.
func (r *Runtime) allowLinked(base, dir string) error {
	stored, _, err := r.credentials()
	if err != nil {
		return err
	}
	if _, known := stored.Installations[base]; known {
		return nil
	}

	path := filepath.Join(dir, linkDir, linkFile)
	refusal := failf(codeUsage, "%s names an installation this machine has not signed in to: %s", path, base)
	if credential := r.environmentCredential(); credential != "" {
		return refusal.withHint("the link file is committed, so it may choose between installations " +
			"`kitchen login` has stored but not introduce one while " + credential + " is set — a commit " +
			"could otherwise send that credential to a host of its own choosing. Say which installation " +
			"this is with --api " + base + " or KITCHEN_API (CI should always set it), or run " +
			"`kitchen login --api " + base + "` on a machine that trusts it")
	}
	if r.noInput || !r.StdinTerminal {
		return refusal.withHint("pass --api " + base + ", set KITCHEN_API, or run " +
			"`kitchen login --api " + base + "` — a committed link file may choose between installations " +
			"this machine has signed in to, and not introduce one")
	}

	answer, err := ask(r, fmt.Sprintf("%s wants to talk to %s, which this machine has not signed in to. "+
		"Trust it? [y/N]", path, base), "--api")
	if err != nil {
		return err
	}
	if !affirmative(answer) {
		return refusal.withHint("pass --api " + base + " or run `kitchen login --api " + base + "` to accept " +
			"that installation")
	}
	return nil
}

// environmentCredential names the credential the environment is carrying, and
// is empty when it carries none. It is a name rather than a value because it
// is only ever read to say in a sentence what is at stake.
func (r *Runtime) environmentCredential() string {
	for _, name := range []string{"KITCHEN_TOKEN", "KITCHEN_API_KEY"} {
		if r.env(name) != "" {
			return name
		}
	}
	return ""
}

// normalizeAPI accepts what a person will actually type — a bare hostname, a
// URL with a trailing slash, a URL with /api/v1 already on the end — and
// answers the base the client wants. Being liberal here costs a few lines and
// removes the single most likely reason a first command fails.
func normalizeAPI(value string) string {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	value = strings.TrimRight(value, "/")
	value = strings.TrimSuffix(value, apiPrefix)
	return strings.TrimRight(value, "/")
}

// httpClient is the one HTTP client the process uses, so connections are
// reused across the several requests a command like `deploy` makes.
func (r *Runtime) httpClient() *http.Client {
	if r.http == nil {
		r.http = &http.Client{}
	}
	return r.http
}

// client is an authenticated client for the resolved installation.
func (r *Runtime) client() (*client, error) {
	base, err := r.apiURL()
	if err != nil {
		return nil, err
	}
	c := newClient(base, r)
	c.http = r.httpClient()
	return c, nil
}

// bearer is the token every request carries, resolved in the order that puts
// the most explicit thing first:
//
//  1. KITCHEN_TOKEN — a token somebody already has. CI that exchanges its own
//     key, a token copied out of a browser session, a test harness.
//  2. The cached token for this installation, while it is still valid. A
//     script running twenty commands exchanges once.
//  3. KITCHEN_API_KEY, then the stored key: exchanged at the issuer for a
//     short-lived token, which is then cached.
//
// The key never reaches the operator on any of these paths, which is the whole
// point of the exchange: revocation stays at the identity provider, and a CLI
// that leaks its credential leaks one that can be deleted in one place.
func (r *Runtime) bearer(ctx context.Context) (string, error) {
	if token := r.env("KITCHEN_TOKEN"); token != "" {
		return token, nil
	}

	base, err := r.apiURL()
	if err != nil {
		return "", err
	}
	stored, dir, err := r.credentials()
	if err != nil {
		return "", err
	}
	current := stored.Installations[base]

	if current != nil && current.Token != "" && !expiring(current.TokenExpiresAt) {
		return current.Token, nil
	}

	key := r.env("KITCHEN_API_KEY")
	if key == "" && current != nil {
		key = current.APIKey
	}
	if key == "" {
		return "", fail(codeUnauthenticated, "not signed in to "+base).
			withHint("run `kitchen login --api " + base + "`, or set KITCHEN_API_KEY " +
				"(an API key from a project's People tab) or KITCHEN_TOKEN")
	}

	issuer, err := r.issuerFor(ctx, base, current)
	if err != nil {
		return "", err
	}
	token, err := exchange(ctx, r.httpClient(), issuer, key)
	if err != nil {
		return "", err
	}

	// Cache it. A token is not a credential anybody has to keep — losing it
	// costs one request — but keeping it is what stops a twenty-command script
	// asking the identity provider twenty times.
	if current == nil {
		current = &installation{}
		stored.Installations[base] = current
	}
	current.Issuer, current.Token, current.TokenExpiresAt = issuer, token, tokenExpiry(token)
	if err := saveCredentials(dir, stored); err != nil {
		// A machine that cannot write the cache can still run the command; it
		// just exchanges every time. Failing here would turn a read-only home
		// directory into a platform that cannot be used at all.
		r.printer().note("could not cache the token: %v", err)
	}
	return token, nil
}

// issuerFor is where this installation's tokens come from: what login recorded,
// or what /config.json says — the same public document the dashboard reads.
//
// The key is sent to whatever this answers, so an issuer that has not been
// pinned by `kitchen login` is taken from that document only when it is on the
// API's own site. An installation federated to an identity provider somewhere
// else is a real thing (the operator may spell out a whole URL), and it is
// signed in to once with --api naming the platform — which pins the issuer,
// after which this never asks the document again.
func (r *Runtime) issuerFor(ctx context.Context, base string, current *installation) (string, error) {
	if current != nil && current.Issuer != "" {
		return current.Issuer, nil
	}
	probe := newClient(base, staticToken(""))
	probe.http = r.httpClient()
	config, err := probe.discover(ctx)
	if err != nil {
		return "", err
	}
	if config.Issuer == "" {
		return "", fail(codeUnauthenticated, base+" has no identity provider configured").
			withHint("an installation running with auth.enabled=false answers 401 on every endpoint; " +
				"there is nothing for the CLI to sign in to")
	}
	if !sameSite(base, config.Issuer) {
		return "", failf(codeUnauthenticated, "%s names an identity provider on another site: %s",
			base+configPath, config.Issuer).
			withHint("the API key is handed to the issuer, so the CLI will not take an off-site one out of " +
				"an unauthenticated document. Run `kitchen login --api " + base + "`, which records this " +
				"installation's issuer once and uses the recorded one from then on")
	}
	return config.Issuer, nil
}

// sameSite reports whether an issuer is on the API's own site: the same host,
// or another name under its registrable domain — auth.example.com for an API
// on kitchen.example.com, which is where the chart puts it by default.
//
// Registrable is the public suffix list's answer rather than "the last two
// labels", so example.co.uk is a site and co.uk is not one.
func sameSite(base, issuer string) bool {
	left, right := hostOf(base), hostOf(issuer)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	leftSite, err := publicsuffix.EffectiveTLDPlusOne(left)
	if err != nil {
		return false
	}
	rightSite, err := publicsuffix.EffectiveTLDPlusOne(right)
	if err != nil {
		return false
	}
	return leftSite == rightSite
}

// hostOf is a URL's hostname, lowercased and without its port. Anything that
// does not parse as a URL with a host has none, which every caller reads as
// "not the same site".
func hostOf(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// expiring reports whether a cached token is gone or about to be. The minute
// of margin is for the request it is about to be used on: a token that expires
// while it is in flight is refused, and the retry would be indistinguishable
// from a revoked credential.
func expiring(at *time.Time) bool {
	return at == nil || time.Now().Add(time.Minute).After(*at)
}

// projectName resolves which project a command is about: the flag, the
// environment, then the working directory's link.
//
// The failure is the one a first-time caller is most likely to see, so it
// names all three ways out rather than only the one the author had in mind.
func (r *Runtime) projectName() (string, error) {
	if name := strings.TrimSpace(r.projectFlag); name != "" {
		return name, nil
	}
	if name := r.env("KITCHEN_PROJECT"); name != "" {
		return name, nil
	}
	linked, _, err := r.link()
	if err != nil {
		return "", err
	}
	if linked != nil {
		return linked.Project, nil
	}
	return "", fail(codeNotLinked, "no project: this directory is not linked to one").
		withHint("run `kitchen link` here, pass --project <name>, or set KITCHEN_PROJECT")
}

// context bounds a command by --timeout when one was given. Zero means no
// bound, which is what a followed build wants by default: it ends when the
// build does.
func (r *Runtime) context(parent context.Context) (context.Context, context.CancelFunc) {
	if r.timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, r.timeout)
}
