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

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Choice is one row of the picker. Detail is the second line under the name —
// it is not called Description because the interface bubbles' list wants needs
// a *method* of that name, and Go will not let a field and a method share one.
type Choice struct {
	Name   string
	Detail string
}

// The three methods bubbles' list wants of an item. Filtering is on the name,
// because that is what somebody types when they know which project they mean.
func (c Choice) FilterValue() string { return c.Name }
func (c Choice) Title() string       { return c.Name }
func (c Choice) Description() string { return c.Detail }

// picker is the model: a bubbles list, and the choice it ends with.
type picker struct {
	list   list.Model
	chosen string
}

func (p picker) Init() tea.Cmd { return nil }

func (p picker) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		// Leave a couple of lines for whatever the shell has already drawn.
		p.list.SetSize(message.Width, min(message.Height-2, 20))
		return p, nil
	case tea.KeyMsg:
		// While the filter is being typed into, every key belongs to the
		// filter — including "q", which is otherwise how somebody leaves.
		if p.list.FilterState() == list.Filtering {
			break
		}
		switch message.String() {
		case "enter":
			if choice, ok := p.list.SelectedItem().(Choice); ok {
				p.chosen = choice.Name
			}
			return p, tea.Quit
		case "ctrl+c", "esc", "q":
			return p, tea.Quit
		}
	}

	updated, cmd := p.list.Update(message)
	p.list = updated
	return p, cmd
}

func (p picker) View() string { return p.list.View() }

// Pick runs the picker and answers what was chosen, or "" if nobody chose
// anything.
//
// It draws on `out` — which the CLI passes as stderr — so that a picker never
// lands in the output somebody is capturing. The caller has already decided
// there is a terminal here: nothing in this package checks, because "is
// anybody watching" is a question about the whole invocation (--json, a pipe,
// --no-input) rather than about a widget.
func Pick(in io.Reader, out io.Writer, title string, choices []Choice) (string, error) {
	items := make([]list.Item, 0, len(choices))
	for _, choice := range choices {
		items = append(items, choice)
	}

	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(accentColour).BorderForeground(accentColour)
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(subtleColour).BorderForeground(accentColour)

	model := picker{list: list.New(items, delegate, 0, min(len(choices)*3+6, 20))}
	model.list.Title = title
	model.list.Styles.Title = lipgloss.NewStyle().Bold(true).Foreground(accentColour)
	model.list.SetShowStatusBar(false)

	finished, err := tea.NewProgram(model, tea.WithInput(in), tea.WithOutput(out)).Run()
	if err != nil {
		return "", fmt.Errorf("drawing the picker: %w", err)
	}
	chosen, _ := finished.(picker)
	return chosen.chosen, nil
}
