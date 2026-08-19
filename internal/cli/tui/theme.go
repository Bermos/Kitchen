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

// Package tui is what `kitchen` looks like when a person is watching: the
// lipgloss styles the whole CLI renders through, and the two Bubble Tea
// programs it runs when there is a terminal to run them in — the deploy
// follower and the project picker.
//
// Nothing in here is ever on the machine-readable path. A Styles built with
// colour off renders every string unchanged (a zero lipgloss.Style is the
// identity), and the CLI only starts a Bubble Tea program when stdout is a
// terminal and --json is off, so the two programs below cannot appear in a
// pipe, in CI, or in front of anything parsing the output.
package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The palette. Adaptive so the CLI is legible on a light terminal as well as a
// dark one — the dashboard's own dark background is not something a terminal
// shares.
var (
	accentColour = lipgloss.AdaptiveColor{Light: "#2f5fd0", Dark: "#7da6ff"}
	subtleColour = lipgloss.AdaptiveColor{Light: "#6b7280", Dark: "#8b929e"}
	okColour     = lipgloss.AdaptiveColor{Light: "#11793f", Dark: "#4ade80"}
	warnColour   = lipgloss.AdaptiveColor{Light: "#a15c07", Dark: "#fbbf24"}
	badColour    = lipgloss.AdaptiveColor{Light: "#b3261e", Dark: "#f87171"}
)

// Styles is every style the CLI renders through, resolved once.
//
// It is a value rather than a package of globals so that "no colour" is a
// Styles somebody made rather than a flag every render site has to remember to
// check: New(false) returns zero styles, and a zero lipgloss.Style renders its
// argument unchanged.
type Styles struct {
	// Colour reports whether this Styles paints anything, for the few places
	// that choose a different *layout* rather than a different colour — a
	// table's rules, a spinner frame.
	Colour bool

	Title  lipgloss.Style
	Accent lipgloss.Style
	Subtle lipgloss.Style
	Key    lipgloss.Style
	OK     lipgloss.Style
	Warn   lipgloss.Style
	Bad    lipgloss.Style
	Header lipgloss.Style
}

// New builds the styles. colour is false whenever the CLI is not writing to a
// terminal, --plain was passed, or the output is JSON.
func New(colour bool) Styles {
	if !colour {
		return Styles{}
	}
	return Styles{
		Colour: true,
		Title:  lipgloss.NewStyle().Bold(true),
		Accent: lipgloss.NewStyle().Foreground(accentColour),
		Subtle: lipgloss.NewStyle().Foreground(subtleColour),
		Key:    lipgloss.NewStyle().Foreground(subtleColour),
		OK:     lipgloss.NewStyle().Foreground(okColour),
		Warn:   lipgloss.NewStyle().Foreground(warnColour),
		Bad:    lipgloss.NewStyle().Foreground(badColour),
		Header: lipgloss.NewStyle().Bold(true).Foreground(subtleColour),
	}
}

// Phase paints one of the platform's phase words by what it means, so a wall
// of build and environment rows can be read without reading every word:
// Succeeded and Live are the same green, Failed and Degraded the same red.
func (s Styles) Phase(phase string) string {
	switch phase {
	case "Succeeded", "Live", "Ready", "True":
		return s.OK.Render(phase)
	case "Failed", "Degraded", "Cancelled", "False":
		return s.Bad.Render(phase)
	case "Running", "Deploying", "Queued", "Pending", "Terminating":
		return s.Warn.Render(phase)
	default:
		return phase
	}
}

// Level paints a log line's severity. The collector has already folded it to
// lower case, so this switch is over the whole vocabulary there is.
func (s Styles) Level(level string) string {
	switch level {
	case "error", "fatal":
		return s.Bad.Render(level)
	case "warn", "warning":
		return s.Warn.Render(level)
	case "debug", "trace":
		return s.Subtle.Render(level)
	default:
		return level
	}
}

// Table lays rows out under their headings, padded to the widest cell in each
// column. It draws no rules and no borders: the output is meant to survive
// being piped into `grep` as readably as it reads on a terminal, and a box
// drawing character in a pipe is noise somebody has to strip.
//
// Cells are pre-rendered strings, so a caller may colour one — the width is
// measured with lipgloss.Width, which counts what will be displayed rather
// than the escape sequences that paint it.
func (s Styles) Table(headers []string, rows [][]string) string {
	widths := make([]int, len(headers))
	for i, heading := range headers {
		widths[i] = lipgloss.Width(heading)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && lipgloss.Width(cell) > widths[i] {
				widths[i] = lipgloss.Width(cell)
			}
		}
	}

	out := &strings.Builder{}
	line := func(cells []string, style lipgloss.Style) {
		for i, cell := range cells {
			if i > 0 {
				out.WriteString("  ")
			}
			padded := cell + strings.Repeat(" ", max(0, widths[i]-lipgloss.Width(cell)))
			// The last column is never padded: trailing spaces on every line
			// are what makes copied output messy.
			if i == len(cells)-1 {
				padded = cell
			}
			out.WriteString(style.Render(padded))
		}
		out.WriteString("\n")
	}

	line(headers, s.Header)
	for _, row := range rows {
		line(row, lipgloss.NewStyle())
	}
	return out.String()
}
