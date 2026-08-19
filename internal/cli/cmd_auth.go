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
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// Signing in.
//
// **The CLI authenticates with an API key**, exchanged at the identity
// provider for the short-lived token the API actually sees — the flow
// docs/API.md documents for CI, used here for a laptop as well. The key never
// reaches the operator, so revoking one is one delete at the issuer and the
// operator has nothing to invalidate.
//
// **There is no browser login yet, and that is a fact about the identity
// provider rather than a gap here.** Kitchen's issuer is better-auth with the
// OAuth provider plugin, and that plugin implements no device authorization
// grant — the flow a CLI would want — so `kitchen login` cannot ask a person
// to approve it in a browser. The other route, a loopback redirect with PKCE,
// needs a client registered with a `http://127.0.0.1:<port>/callback` redirect
// URI, and the plugin refuses a client whose redirect URIs are on more than
// one host (a port is part of the host), so it would have to be seeded for one
// fixed port that may be in use. Both are decisions for auth/ rather than
// things the CLI can assume; see docs/CLI.md, "Signing in".
//
// What that leaves is deliberate rather than a compromise: a key is a machine
// account with a grant on exactly one project, which is the narrowest
// credential the platform can issue — and a CLI is exactly where a too-broad
// token would end up on a laptop.

func newLoginCommand(r *Runtime) *cobra.Command {
	var (
		key      string
		fromFile string
		stdin    bool
	)

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store a credential for a Kitchen installation",
		Long: strings.TrimSpace(`
Store an API key for an installation and check that it works.

The key is exchanged at the platform's identity provider for a short-lived
token, which is what the API sees; the key itself never reaches the operator.
Issue one from a project's Keys tab in the dashboard, or with
POST /projects/{name}/keys — it is a machine account with a role on that one
project, so a key can deploy the project it was made for and nothing else.

There is no browser sign-in: the platform's identity provider implements no
device authorization grant, so there is nothing for the CLI to open a browser
for. See docs/CLI.md.

The credential is written to auth.json in the user's configuration directory,
readable by nobody else. KITCHEN_TOKEN (a token somebody already exchanged) and
KITCHEN_API_KEY both work without logging in at all, which is what CI should
use.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			return login(commandContext(cmd), r, key, fromFile, stdin)
		}),
	}

	cmd.Flags().StringVar(&key, "api-key", "",
		"the API key itself. Prefer --api-key-stdin or --api-key-file: an argument is visible in the process list [KITCHEN_API_KEY]")
	cmd.Flags().StringVar(&fromFile, "api-key-file", "", "read the API key from a file")
	cmd.Flags().BoolVar(&stdin, "api-key-stdin", false, "read the API key from stdin")
	mustAnnotate(cmd.Flags(), "api-key", "KITCHEN_API_KEY")

	return describe(cmd, meta{
		Calls:  []string{"GET /config.json", "GET <issuer>/token", "GET /api/v1/me"},
		Output: output{Mode: outputDocument, Kind: "account", Note: "who the credential turned out to belong to"},
		Needs:  needs{Auth: false},
		Examples: []example{
			{"Sign in with a key from a file, without a terminal",
				"kitchen login --api https://kitchen.example.com --api-key-file ./key --json"},
			{"Sign in with a key on stdin",
				"printf %s \"$KEY\" | kitchen login --api https://kitchen.example.com --api-key-stdin --json"},
		},
	})
}

func login(parent context.Context, r *Runtime, key, fromFile string, fromStdin bool) error {
	base, err := loginTarget(r)
	if err != nil {
		return err
	}
	key, err = readKey(r, key, fromFile, fromStdin)
	if err != nil {
		return err
	}

	ctx, cancel := r.context(parent)
	defer cancel()

	probe := newClient(base, staticToken(""))
	probe.http = r.httpClient()
	config, err := probe.discover(ctx)
	if err != nil {
		return err
	}
	if config.Issuer == "" {
		return fail(codeUnauthenticated, base+" has no identity provider configured").
			withHint("an installation running with auth.enabled=false refuses every endpoint; " +
				"there is nothing to sign in to")
	}

	token, err := exchange(ctx, r.httpClient(), config.Issuer, key)
	if err != nil {
		return err
	}

	// A key that exchanges is not yet a key that works: it authenticates, and
	// whether the project granted it anything is a separate question. Asking
	// the API who this is answers both, and means `login` never reports
	// success for a credential the next command will be refused with.
	authenticated := newClient(base, staticToken(token))
	authenticated.http = r.httpClient()
	who, err := authenticated.me(ctx)
	if err != nil {
		return err
	}

	stored, dir, err := r.credentials()
	if err != nil {
		return err
	}
	expiry := tokenExpiry(token)
	stored.Installations[base] = &installation{
		Issuer:         config.Issuer,
		APIKey:         key,
		Token:          token,
		TokenExpiresAt: expiry,
		Account:        accountName(who),
	}
	stored.Current = base
	if err := saveCredentials(dir, stored); err != nil {
		return err
	}

	return r.printer().document(who, func(s tui.Styles) string {
		return fmt.Sprintf("%s %s on %s\n", s.OK.Render("Signed in as"),
			s.Title.Render(accountName(who)), s.Accent.Render(base))
	})
}

// loginTarget is which installation is being signed in to. It is the one thing
// login cannot infer from anything already stored, so it asks when it can and
// names the flag when it cannot.
func loginTarget(r *Runtime) (string, error) {
	if base, err := r.apiURL(); err == nil {
		return base, nil
	}
	answer, err := ask(r, "Kitchen URL (e.g. https://kitchen.example.com):", "--api")
	if err != nil {
		return "", err
	}
	if answer == "" {
		return "", fail(codeUsage, "no installation given").withHint("pass --api https://kitchen.<your-domain>")
	}
	r.base = normalizeAPI(answer)
	return r.base, nil
}

// readKey finds the credential, in the order that keeps it off the process
// list where it can: a file, stdin, the environment, the flag, and only then a
// prompt — which is masked when the terminal can be.
func readKey(r *Runtime, key, fromFile string, fromStdin bool) (string, error) {
	switch {
	case fromFile != "":
		body, err := os.ReadFile(fromFile)
		if err != nil {
			return "", fail(codeUsage, "reading "+fromFile+": "+err.Error())
		}
		return strings.TrimSpace(string(body)), nil
	case fromStdin:
		body, err := io.ReadAll(io.LimitReader(r.Stdin, 1<<16))
		if err != nil {
			return "", fail(codeUsage, "reading the key from stdin: "+err.Error())
		}
		return strings.TrimSpace(string(body)), nil
	case key != "":
		return key, nil
	}
	if fromEnv := r.env("KITCHEN_API_KEY"); fromEnv != "" {
		return fromEnv, nil
	}

	if r.noInput || !r.StdinTerminal {
		return "", fail(codeUsage, "no API key").
			withHint("pass --api-key-file, --api-key-stdin or --api-key, or set KITCHEN_API_KEY")
	}
	_, _ = fmt.Fprint(r.Stderr, "API key: ")
	secret, err := readSecret(r.Stdin)
	_, _ = fmt.Fprintln(r.Stderr)
	if err != nil {
		return "", fail(codeUsage, "reading the key: "+err.Error())
	}
	if secret == "" {
		return "", fail(codeUsage, "no API key given")
	}
	return secret, nil
}

// readSecret reads a line without echoing it, when stdin is a terminal that
// can be put into that state. Anything else falls back to a plain read: on a
// stream that is not a terminal there is nothing to echo it to.
func readSecret(in io.Reader) (string, error) {
	file, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return readLine(in)
	}
	typed, err := term.ReadPassword(int(file.Fd()))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(typed)), nil
}

func newLogoutCommand(r *Runtime) *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Forget the credential for an installation",
		Long: strings.TrimSpace(`
Remove this machine's stored credential for an installation.

It forgets; it does not revoke. The key itself still exists at the identity
provider until somebody deletes it there — DELETE /projects/{name}/keys/{key},
or the project's Keys tab — which is the thing to do for a key that has
leaked.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			return logout(r, all)
		}),
	}
	cmd.Flags().BoolVar(&all, "all", false, "forget every installation, not just this one")

	return describe(cmd, meta{
		Output: output{Mode: outputDocument, Kind: "forgotten"},
		Needs:  needs{},
		Examples: []example{
			{"Forget one installation", "kitchen logout --api https://kitchen.example.com --json"},
			{"Forget all of them", "kitchen logout --all --json"},
		},
	})
}

// forgotten is what logout answers with: which installations this machine no
// longer holds a credential for.
type forgotten struct {
	Forgotten []string `json:"forgotten"`
}

func logout(r *Runtime, all bool) error {
	stored, dir, err := r.credentials()
	if err != nil {
		return err
	}

	answer := forgotten{Forgotten: []string{}}
	if all {
		for base := range stored.Installations {
			answer.Forgotten = append(answer.Forgotten, base)
		}
		stored.Installations = map[string]*installation{}
		stored.Current = ""
	} else {
		base, err := r.apiURL()
		if err != nil {
			return err
		}
		if _, ok := stored.Installations[base]; ok {
			answer.Forgotten = append(answer.Forgotten, base)
			delete(stored.Installations, base)
		}
		if stored.Current == base {
			stored.Current = ""
		}
	}
	slices.Sort(answer.Forgotten)

	if err := saveCredentials(dir, stored); err != nil {
		return err
	}
	return r.printer().document(answer, func(s tui.Styles) string {
		if len(answer.Forgotten) == 0 {
			return "No stored credential to forget.\n"
		}
		return fmt.Sprintf("Forgot the credential for %s.\n", s.Accent.Render(strings.Join(answer.Forgotten, ", ")))
	})
}

func newWhoamiCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Who the stored credential is, and what it may do",
		Long: strings.TrimSpace(`
Ask the platform who this credential belongs to.

The answer is the account's subject, address and platform role. What it may do
to a project is the project's own answer — every project payload carries the
calling account's role on it, so "kitchen projects" is the other half of this
question.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			who, err := client.me(ctx)
			if err != nil {
				return err
			}
			return r.printer().document(who, func(s tui.Styles) string {
				return fmt.Sprintf("%s\n%s %s\n%s %s\n",
					s.Title.Render(accountName(who)),
					s.Key.Render("subject      "), who.Subject,
					s.Key.Render("platform role"), who.PlatformRole)
			})
		}),
	}
	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/me"},
		Output:   output{Mode: outputDocument, Kind: "account"},
		Needs:    needs{Auth: true},
		Examples: []example{{"Check the credential works", "kitchen whoami --json"}},
	})
}

// accountName is what to call somebody: their address when the token carried
// one, and their subject when it did not — a machine account has no address
// worth reading, and an opaque identifier is better than an empty string.
func accountName(who *account) string {
	switch {
	case who == nil:
		return ""
	case who.Email != "":
		return who.Email
	case who.Name != "":
		return who.Name
	default:
		return who.Subject
	}
}
