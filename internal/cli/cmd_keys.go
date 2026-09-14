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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// `kitchen keys` — the credentials this account holds for its own automation
// (#593).
//
// Two of the three things one does with a personal key are here, and the third
// deliberately is not. **Issuing one needs a browser sign-in**: a personal key
// carries every role its holder has, so a credential that could mint one would
// be minting its own successor, and the API asks for a token issued to the
// dashboard's own OAuth client — which is the one thing this CLI can never
// hold, since what it holds is a key (docs/CLI.md, "Signing in"). That is not
// a gap in the CLI: it is the rule that keeps the chain starting at somebody
// typing a password.
//
// So the shape is: make one in the dashboard, `kitchen login` with it, and
// from then on this is where you see what you are holding and take one back.
// Revoking is here rather than the dashboard's alone because it is the thing
// somebody needs at the worst moment — a key pasted into the wrong window —
// and because a key can revoke itself, which is exactly what should happen
// then.

func newKeysCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "keys",
		Aliases: []string{"key"},
		Short:   "The personal keys this account holds",
		Long: strings.TrimSpace(`
Your own keys: the credentials you sign your own automation with.

A personal key carries your identity — every project role you hold, and the
operator role if you have it — so it does what you would do. It expires, it is
listed here by name, and revoking one is one command.

Issuing one is the dashboard's: Account -> Personal keys. It needs a browser
sign-in, because a credential that could issue a personal key would be issuing
a copy of you, and the credential this CLI holds is a credential. Once you have
one, "kitchen login --api-key-stdin" stores it and everything else works as it
always did.

"kitchen whoami" says what the stored credential is and what it holds.`),
	}
	cmd.AddCommand(newKeysListCommand(r), newKeysRevokeCommand(r))

	return describe(cmd, meta{
		Output:   output{Mode: outputNone},
		Needs:    needs{},
		Examples: []example{{"What this account holds", "kitchen keys list --json"}},
	})
}

func newKeysListCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "The personal keys this account holds, and when each lapses",
		Long: strings.TrimSpace(`
List this account's personal keys.

Names, prefixes, when each was made, when it was last used and when it stops
working — never a value: the key exists in the one response that created it and
nowhere else, so a lost key is revoked and reissued rather than looked up.

"lastUsed" is what answers "is this still the credential my pipeline holds".
A key listed as expired has lapsed and is refused already; it disappears the
next time anything presents it.

A credential that is not a person's — a project API key, a platform credential
— holds no personal keys and gets an empty list rather than a refusal.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			keys, err := client.personalKeys(ctx)
			if err != nil {
				return err
			}
			answer := list[personalKey]{Items: keys}
			if answer.Items == nil {
				answer.Items = []personalKey{}
			}
			return r.printer().document(answer, func(s tui.Styles) string {
				return renderPersonalKeys(s, answer.Items)
			})
		}),
	}
	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/me/keys"},
		Output:   output{Mode: outputDocument, Kind: "personalKeyList"},
		Needs:    needs{Auth: true},
		Examples: []example{{"What this account holds", "kitchen keys list --json"}},
	})
}

func newKeysRevokeCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "revoke NAME",
		Aliases: []string{"rm", "delete"},
		Short:   "Revoke one of this account's personal keys",
		Long: strings.TrimSpace(`
Revoke a personal key by name.

It stops working immediately, everywhere: the key is verified at the identity
provider, so there is nothing cached anywhere for it to keep working against.
A token somebody already exchanged it for lives out its few minutes, which is
the same bargain every credential on this platform makes.

A key may revoke itself. If the one in your hand is the one that leaked, this
is the command to run with it.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			name := strings.TrimSpace(args[0])
			if err := client.revokePersonalKey(ctx, name); err != nil {
				return err
			}
			// The delete answers 204, so what is left is read back rather
			// than guessed at — the same thing `secret rm` prints, for the
			// same reason.
			remaining, err := client.personalKeys(ctx)
			if err != nil {
				return err
			}
			answer := list[personalKey]{Items: remaining}
			if answer.Items == nil {
				answer.Items = []personalKey{}
			}
			return r.printer().document(answer, func(s tui.Styles) string {
				return renderPersonalKeys(s, answer.Items)
			})
		}),
	}
	return describe(cmd, meta{
		Calls:    []string{"DELETE /api/v1/me/keys/{key}", "GET /api/v1/me/keys"},
		Output:   output{Mode: outputDocument, Kind: "personalKeyList", Note: "what is left afterwards"},
		Needs:    needs{Auth: true},
		Examples: []example{{"Take one back", "kitchen keys revoke laptop --json"}},
	})
}

// renderPersonalKeys draws what there is. Never a value — there is none to
// draw.
func renderPersonalKeys(s tui.Styles, keys []personalKey) string {
	if len(keys) == 0 {
		return "No personal keys. Issue one in the dashboard, under Account -> Personal keys.\n"
	}
	rows := make([][]string, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, []string{
			key.Name,
			s.Subtle.Render(key.Prefix + "…"),
			lastUsed(key),
			expiry(key),
		})
	}
	return s.Table([]string{"NAME", "KEY", "LAST USED", "EXPIRES"}, rows)
}

// lastUsed is when a key was last exchanged for a token, in words — and
// "never" for one nothing has used, which is a different answer from a date
// long ago.
func lastUsed(key personalKey) string {
	if key.LastUsed == nil {
		return "never"
	}
	return key.LastUsed.Local().Format(time.DateOnly)
}

// expiry says when a key stops working, and says so plainly for one that
// already has: a list that showed a lapsed key as a date in the past would
// leave every reader comparing it against today.
func expiry(key personalKey) string {
	if key.Expired {
		return "expired"
	}
	if key.Expires.IsZero() {
		return "—"
	}
	return key.Expires.Local().Format(time.DateOnly)
}
