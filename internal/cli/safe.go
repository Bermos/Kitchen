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
	"unicode/utf8"
)

// Text the platform did not write.
//
// A log line is whatever an application printed, and a build's output is
// whatever a repository's Dockerfile printed — neither is the platform's
// words, and both are read by an operator whose terminal will act on what is
// in them. `ESC [ 2 J` clears it; `ESC ] 0 ; … BEL` rewrites its title; `ESC
// [ … m` paints the rest of the tail in whatever colour makes a failure look
// like a success. A repository nobody trusts can print any of them, so
// `kitchen logs` on somebody's pull request is a terminal an untrusted party
// writes to.
//
// So text the platform relays is stripped of everything that is not a
// printable character before a person is shown it. Newline and tab stay,
// because a multi-line line is still one line and an aligned one is still
// aligned; everything else in the C0 and C1 control ranges goes, escape
// included, which takes every sequence with it — a sequence without its
// introducer is inert text.
//
// Two things are deliberately *not* stripped:
//
//   - **`--json` output.** It is the answer, byte for byte: a caller that
//     asked for JSON asked for what the platform said, and `jq` does not act
//     on an escape sequence. Mangling it would also break the one promise
//     the machine surface makes, which is that the document is the platform's
//     and not a rendering of it.
//   - **the CLI's own styling.** The colours a person sees are written by
//     tui.Styles around this text, never inside it, which is why this is
//     applied to the fields rather than to the finished line.
func safeText(s string) string {
	if !strings.ContainsFunc(s, unprintable) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unprintable(r) {
			return -1
		}
		return r
	}, s)
}

// unprintable reports the runes a terminal reads as an instruction rather
// than as text: the C0 controls (except the two whitespace ones that carry
// meaning), DEL, and the C1 controls — which matter because `ESC [` has a
// single-byte equivalent at U+009B that a filter looking only for U+001B
// would let through.
//
// utf8.RuneError is in the list because it is what a byte that is not valid
// UTF-8 decodes as, and a raw 0x9b is exactly that: dropping it means the
// byte never reaches the terminal that would have read it as an introducer.
// It costs a legitimate U+FFFD, which is a replacement character somebody
// else's decoder already gave up on.
func unprintable(r rune) bool {
	switch r {
	case '\n', '\t':
		return false
	case utf8.RuneError:
		return true
	}
	return r < 0x20 || (r >= 0x7f && r <= 0x9f)
}

// safeBytes is safeText over a body the platform relayed rather than wrote —
// `kitchen api`'s answer, when it is not JSON and a person is watching.
func safeBytes(b []byte) []byte {
	return []byte(safeText(string(b)))
}
