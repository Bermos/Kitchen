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

package tui

import (
	"fmt"
	"io"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// Event is one thing that happened while something was being followed,
// rendered by the caller.
//
// The caller renders rather than this package, because the same events go to a
// pipe as NDJSON when nobody is watching — one description of what happened,
// two ways of showing it, and no chance of the terminal seeing something the
// JSON did not.
type Event struct {
	// Line is a line to print above the status block. It scrolls into the
	// terminal's own scrollback, which is what keeps a followed build's output
	// readable after the command has finished.
	Line string
	// Phase replaces what the status block says is happening.
	Phase string
	// Done ends the program. The channel closing does the same thing, so a
	// caller that simply stops sending is handled too.
	Done bool
}

// follower is the status block: a spinner, what is happening, and how long it
// has been happening for. Everything else scrolls past above it.
type follower struct {
	spinner spinner.Model
	styles  Styles
	events  <-chan Event
	title   string
	phase   string
	started time.Time
	done    bool
}

func (f follower) Init() tea.Cmd {
	return tea.Batch(f.spinner.Tick, waitFor(f.events))
}

func (f follower) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case Event:
		if message.Done {
			f.done = true
			return f, tea.Quit
		}
		commands := []tea.Cmd{waitFor(f.events)}
		if message.Line != "" {
			// Printed above the program rather than into it: the line belongs
			// to the terminal's scrollback, not to a frame that will be
			// redrawn over.
			commands = append(commands, tea.Println(message.Line))
		}
		if message.Phase != "" {
			f.phase = message.Phase
		}
		return f, tea.Batch(commands...)

	case spinner.TickMsg:
		updated, cmd := f.spinner.Update(message)
		f.spinner = updated
		return f, cmd

	case tea.KeyMsg:
		if message.String() == "ctrl+c" {
			// The process's own signal handling ends the work; quitting here
			// only stops drawing. Both converge, and the command reports that
			// it was interrupted rather than that anything failed.
			return f, tea.Quit
		}
	}
	return f, nil
}

func (f follower) View() string {
	if f.done {
		// Nothing: the caller prints the summary once the program has let go
		// of the terminal, so the last thing on screen is the answer rather
		// than a spinner frozen mid-turn.
		return ""
	}
	elapsed := time.Since(f.started).Truncate(time.Second)
	return fmt.Sprintf("%s %s %s %s\n",
		f.spinner.View(),
		f.styles.Title.Render(f.title),
		f.styles.Phase(f.phase),
		f.styles.Subtle.Render(elapsed.String()))
}

// waitFor turns the next event on the channel into a message. A closed channel
// is the end of the work, which is the same message as an explicit Done.
func waitFor(events <-chan Event) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return Event{Done: true}
		}
		return event
	}
}

// Follow draws the status block on `out` until the events channel closes.
//
// It returns when there is nothing left to draw; whatever produced the events
// is the caller's to wait for. Errors here are terminal-drawing errors alone —
// what was being followed reports its own outcome.
func Follow(in io.Reader, out io.Writer, styles Styles, title string, events <-chan Event) error {
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	spin.Style = styles.Accent

	model := follower{
		spinner: spin,
		styles:  styles,
		events:  events,
		title:   title,
		phase:   "starting",
		started: time.Now(),
	}
	if _, err := tea.NewProgram(model, tea.WithInput(in), tea.WithOutput(out)).Run(); err != nil {
		return fmt.Errorf("drawing the follower: %w", err)
	}
	return nil
}
