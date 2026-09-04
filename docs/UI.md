# The dashboard's design guide

The dashboard is one product with two audiences and about forty screens. This
is what makes them look like one product: the frame every screen is built in,
the scale everything inside it is measured against, and the one rule that
decides which of the two audiences a screen is talking to.

It exists because none of that was written down, and a UI with nothing written
down drifts — not by anybody deciding differently, but by each new screen
guessing at a shape the last one never stated. By the time it was noticed the
dashboard had three page widths, three vertical rhythms, four table paddings,
two weights for the same heading, and three screens showing the operator's
answers to somebody who had asked for the developer's.

So: the rules are here, and **the ones a machine can hold are held by
[`ui/src/lib/design.test.ts`](../ui/src/lib/design.test.ts)**, which runs in
`npm test` and therefore in CI. A rule that is not enforced stops being true,
and a rule that has stopped being true is worse than no rule, because the next
person reads it and believes it.

## The mode rule

**Role decides what is permitted. Mode decides what is rendered.**

The role half is [docs/AUTH.md](AUTH.md), enforced twice over — the API's route
table, and the dashboard's generated copy of it. The mode half is this
document, and it had exactly one enforcement mechanism until now: whoever wrote
the screen remembering to write `v-if="operatorMode"`.

Four screens remembered. The environment screen's pod table, the crash report's
Kubernetes events and the log screen's cluster switch did not — so an operator
who chose the developer's view kept being handed the operator's answers on the
screens they use most, which is where this guide started.

### What operator content is

Anything that names a Kubernetes object: a Pod, a Node, a namespace, a
manifest, a `status.conditions` row, a cluster Event, a PersistentVolume, a
PersistentVolumeClaim, a StorageClass, a workload the platform runs on its own
behalf. [docs/SCOPE.md](SCOPE.md) is the reason —

> The developer should never need the words "namespace" or "Deployment".

— and it is not about secrecy. The API decides what may be *read*, by role. This
decides what is *worth reading*, and a Kubernetes noun is the wrong answer to
every question a developer is asking. "Is my app up" is answered by the health
strip, the crash report and the findings; it is not answered better by a pod
name.

### The gate is per screen, not per block

A screen is the developer's or the operator's, and it is whichever one entire.

- **A developer screen** carries no operator content except behind
  [`<OperatorOnly>`](../ui/src/components/OperatorOnly.vue). That element takes
  a slot and renders nothing around it, so it can wrap a table, a table row, a
  heading, or three words in the middle of a sentence.
- **An operator screen** — everything under `/platform`, and connections — is
  operator content throughout, and nothing inside it is gated a second time.
  Those routes stay open to an operator who is in developer mode, because a
  finding's evidence link is a link somebody pastes and it should land where it
  says it does. Gating blocks *within* such a screen is how the settings page
  came to show its top half and not its bottom to an operator who had followed
  a link there.

The corollary, and the case that is easy to miss: **a preference that turns on
operator content is narrowed by the mode too, not only the control that sets
it.** The log screen's cluster switch is hidden in developer mode — and
`?cluster=1` in a pasted URL is ignored there as well, because the switch was
never the only way in. `ObservabilityView.vue` derives the effective value the
way `mode.ts` derives the mode: a preference, narrowed by what the viewer may
act on.

### What the test checks

`design.test.ts` reads the rendered *words* — text nodes, and the attributes
that become text (`title`, `aria-label`, `placeholder`, `label`, `description`,
`empty`, `hint`, `alt`) — and refuses a developer screen that says one of the
Kubernetes nouns outside a gate. An expression like `pod.name` is not caught: a
field name is not a label, and what leaks is a screen *saying* Pod.

Two lists in that file are the escape hatches, and both are meant to be argued
with rather than added to quietly:

- `OPERATOR_COMPONENTS` — a component that is only ever mounted behind a gate
  *and* speaks the vocabulary in its own text. It is deliberately the shortest
  list that works, three entries long, rather than every component an operator
  screen happens to use: a component that is clean today stays checked, and so
  stays clean. Adding one means saying where its gate is.
- `NOT_ABOUT_KUBERNETES` — the handful of phrases that contain one of the words
  and mean something else. Log *clustering* is the standing example.

## The page

The shell owns the page. `AppShell.vue` caps the content column at `110rem`,
pads it, and centres it; a view renders inside that and does not cap itself
again.

```vue
<template>
  <div class="space-y-6">
    <PageHeader title="Nodes" :breadcrumb="[{ label: 'Platform', to: '/platform' }, { label: 'Nodes' }]">
      <template #description>What the cluster is made of.</template>
      <template #actions>…</template>
    </PageHeader>

    …
  </div>
</template>
```

- **One root element**, and its rhythm is `space-y-6`. Sections are spaced by
  the page, not each by itself; a section that wants to sit closer to the one
  above it is one section, not two.
- **One width.** The only cap a view may declare for itself is `max-w-3xl`, and
  it means "this page is a form" — the account screen, a project's settings.
  Everything else is a dashboard and takes the column. A form *inside* a
  dashboard page (the members panel, the environment variables panel) takes the
  same `max-w-3xl`, so there is one form width rather than one per panel.
- **One header.** [`PageHeader`](../ui/src/components/PageHeader.vue) is the
  first thing on every page. It owns the `<h1>`, so no other file writes one.

### The header's parts

| Slot | What goes in it |
|---|---|
| `title` (prop) | The name of the thing. A page name, or an object's own. |
| `breadcrumb` (prop) | The trail. The last entry has no `to`; `mono: true` for an identifier. |
| `description` | **One sentence saying what this screen answers.** Every screen named after a subject owes one — a heading says what a page is called, never what it is for. A screen named after an *object* (a project, a build, an environment) does not: its title is the object, and `meta` carries the object's own facts instead. |
| `badges` | What the title *is*: a phase, an environment type, a classification. |
| `meta` | The small facts under it: a repository, a branch, an age. |
| `actions` | What can be done to the thing named. |
| `freshness` (prop) | The screen's own age and the reader's hold on it, on a screen that polls. Below. |

### The freshness control

**A screen that polls says how old it is, and offers to hold still while it is
read.** The dashboard polls a great deal — five clocks on the overview alone —
and every one of them used to be silent: rows reordered themselves under the
cursor mid-click, and a metrics store that had stopped answering left confident
numbers from twenty minutes ago looking exactly like numbers from four seconds
ago. One control in the header answers both, and it is the header's rather than
each screen's so that the answer is in the same place on every screen.

A screen takes it in one line — `const freshness = useFreshness()`, handed to
`PageHeader` — and everything that polls on that screen reports into it through
`useAsync`, the panels inside the view included. It says one of four things:

| | What it means |
|---|---|
| `Live · 4s ago` | The oldest thing on the screen was fetched four seconds ago. |
| `Paused · 3 changes waiting` | Held while you read; three of the screen's sources have newer data, applied when you resume. |
| `Stale · 22m ago` | A source has failed for longer than its own poll interval. The screen is not being told the truth. |
| `Loading…` | Nothing has answered yet. |

Four decisions inside that, each of which somebody will want to change:

- **The screen is as old as its oldest part.** The age is the oldest of the
  screen's sources, not the newest. The reader is asking whether they can trust
  what is in front of them, and the weakest part is the honest answer.
- **Pause is per screen, and navigating ends it.** It exists so that what
  somebody is reading does not move under the cursor; leaving the screen is
  them saying they have finished reading it. A pause that travelled would be a
  global "stop updating" nobody asked for, on screens whose data they have not
  looked at yet. The shell's own pollers — the sidebar's inventory and the
  platform status at its foot — are outside it entirely (`screen: false`),
  because the sidebar is navigation rather than something being read.
- **Pause expires after five minutes.** A pause that outlives the reading it
  was for *is* the stale screen this control exists to prevent, wearing a label
  that says everything is fine. Five minutes is long enough to read a failing
  build's log excerpt and short enough that a screen left open over lunch is
  live again when its owner comes back. The held data is applied when it
  expires, not thrown away.
- **Stale is a claim, and it is only made when it is true.** A source is stale
  once its failures have outlasted its own poll interval — one missed poll on a
  sixty-second fetch is a blip, a minute of them is a number that has stopped
  being true. A source that has never answered is not stale: there is nothing
  on the screen to be wrong about, and the view's error alert says so.

**A stale source is a screen that stops repeating itself.** The strip goes to
the warning tone, and the numbers it covers fall back to `—` with a banner
naming the real age — `the metrics store did not answer the last 3 polls —
these numbers are from 22:38`. This is the `—` rule below along the time axis:
a number the caller may not know is `—` rather than `0`, and a number that has
stopped being true is `—` rather than the number it used to be.

`design.test.ts` requires the control on every view that calls `usePoll`, and
requires that nothing but `PageHeader` places it.

## Sections inside a page

[`PageSection`](../ui/src/components/PageSection.vue) is the same shape one
level down — a heading, an optional sentence, an optional `id` for a finding's
`?section=` link to scroll to, and the block itself. Where a section is written
out by hand instead, it keeps the heading scale:

| | Size and weight | What it is |
|---|---|---|
| `<h1>` | `text-xl font-semibold` | The page. `PageHeader` writes it; nothing else does. |
| `<h2>` | `text-sm font-medium` | A section of a page. |
| `<h3>` | `text-xs font-medium` | A block within a section. |

The tone (`text-highlighted`, `text-muted`, `text-error`) is free — a danger
zone's heading is red and still a section heading. The size and the weight are
not.

## The attention band

The overview's band is not part of the frame — it is one screen's answer to
one question — but two decisions inside it are written down here, because the
next screen that hoists a problem out of a list will face both and there is no
reason for it to answer them differently.

**What it is:** failing and degraded work leaves the table and is rendered
above it, with the three things a triage needs beside each other — the error
line at its full length (it is the one string that lets somebody decide whether
to act, and it was the one the table truncated), the blast radius as its own
sentence, and the resolving action as a button. The row stays in the table
below, dimmed and marked `in band`: the table is the inventory, and an
inventory with holes in it is worse than one that repeats itself.

- **The cap is five, and the rest fold rather than disappear.** Past five a
  band stops being a band: the thing that was meant to put the worst problem
  where the eye lands becomes its own scrolling list with the inventory pushed
  off the screen behind it. The band says how many it is holding back — `2
  more` — so a sixth problem is never something the screen quietly failed to
  mention.
- **A dismissal lasts until the condition changes, and no longer.** Back on
  the next poll makes the control useless during the ten minutes somebody is
  already fixing the thing; lasting past the failure would hide the *next* one,
  which is the failure mode that stops people trusting a band like this at all.
  So a dismissal is recorded against a signature carrying the error, the
  release and the build: the same failure stays down, a new one comes back. It
  is deliberately not persisted — a dismissal says "I have seen it, I am on
  it", and a reload is somebody asking what is wrong again.

The `conditions` in the expanded row are operator content like any other, and
sit behind `<OperatorOnly>`; the error line and the failing step's output are
the developer's and are not gated.

## Tables

Most of this dashboard is tables, and they are where the drift showed first:
four horizontal paddings, five vertical ones, and headers at a different height
from the bodies under them.

**One gutter.** `px-3`, everywhere, in both `<th>` and `<td>`.

**Three densities, and a table picks one** for its header and its body
together:

| | When |
|---|---|
| `py-2` | The default. Anything a person reads a row at a time. |
| `py-1` | A dense table: an audit trail, an inventory, a ranking inside a panel. |
| `py-0.5` | Log lines and request lines, where the density is the point. |

**Two shapes, and a table is one of them throughout.** A **boxed** table draws
its own edge and every cell carries the `px-3` gutter. A **flush** table sits
inside a block that is already padded — the conditions under a node, the
evidence under a requirement — and takes its left edge from that block, so no
cell sets a left gutter and its columns line up with the prose above them.
Mixing the two puts one column half a gutter out of line.

**Two exceptions, both about cells that are not cells in a column:**

- A `colspan` cell is a block — the "nothing here" line, or a row expanded into
  a panel — and is spaced as one. `py-8` is the empty line's own spacing.
- A cell that indents itself (`pl-10 pr-3`, a nested build row) says both halves
  rather than overriding one of a pair.

**A table that declares a minimum width scrolls in its own container**
(`overflow-x-auto`), never the page. This is the whole reason the content
column is `110rem` rather than a comfortable measure for prose: these screens
are dashboards before they are documents, and a commit subject, a phase, a
duration and a time do not fit in `72rem`.

## Colour

The palette is the tokens in
[`ui/src/assets/main.css`](../ui/src/assets/main.css), and nothing else. No
Tailwind palette classes (`text-blue-500`), no hex literals in a template. The
tokens carry semantics rather than hues — `text-muted` and `text-error` say
what a thing is, `text-neutral-400` says what it looks like — and going through
them is what makes a change of palette one file.

The tones, in the order they get reached for: `text-highlighted` for the thing
itself, `text-toned` for a value, `text-muted` for a label, `text-dimmed` for
something that is present but not being asked about, and `text-error` /
`text-warning` / `text-success` for a state rather than a decoration.

## Identifiers, numbers and prose

`font-mono` for anything that is typed or copied: a name, a hostname, a SHA, a
URL, a duration, a count. Sentence case for everything a person reads. An em
dash — like this — rather than parentheses, which is the voice the rest of this
repository is written in and the dashboard is written in too.

## What is not in this guide

What a screen *says*, how a section is laid out inside itself, which chart it
draws, what it does when it is empty. Those are judgement, and a rule that
pretended to make them would only be in the way. The three things worth saying:

- **An empty answer is an answer, and says why.** "No flow data in this window"
  with the reason under it, not a blank panel.
- **A control nobody may use is not rendered disabled, it is not rendered.**
  Except where saying why is useful, in which case `refusal()` in
  `lib/policy.ts` has the API's own words for it.
- **A number the caller may not know is `—`, never `0`.** The API narrows some
  answers by role (`narrowsAnswer()`), and an absent field means "you may not
  know" rather than "none".

## Changing the guide

Both halves move together. A rule that changes here changes in
`design.test.ts`, and a rule that is loosened there is loosened here with the
reason — otherwise the file drifts into an allowlist and the guide into
folklore. Adding a screen is the cheap case: it inherits all of this from
`PageHeader`, `PageSection` and `OperatorOnly`, and the test tells you the one
thing you forgot.
