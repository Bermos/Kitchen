# The dashboard's design guide

The dashboard is one product with four audiences and about forty screens. This
is what makes them look like one product: the frame every screen is built in,
the scale everything inside it is measured against, and the one rule that
decides which of the four a screen is talking to.

It exists because none of that was written down, and a UI with nothing written
down drifts — not by anybody deciding differently, but by each new screen
guessing at a shape the last one never stated. By the time it was noticed the
dashboard had three page widths, three vertical rhythms, four table paddings,
two weights for the same heading, and one flat navigation listing four
audiences' screens at once.

So: the rules are here, and **the ones a machine can hold are held by
[`ui/src/lib/design.test.ts`](../ui/src/lib/design.test.ts)**, which runs in
`npm test` and therefore in CI. A rule that is not enforced stops being true,
and a rule that has stopped being true is worse than no rule, because the next
person reads it and believes it.

## The scope rule

**The dashboard has four scopes, the address says which one you are in, and
the scope decides what may be on the screen.**

| Scope | Root | Whose |
|---|---|---|
| **Fleet** | `/` | Everybody's, and the only place that spans projects |
| **Project** | `/projects/:name/…` | The developer's, scoped by the address |
| **Platform** | `/platform/…` | The operator's, and where an operator lands on sign-in |
| **Compliance** | `/compliance/…` | The auditor's, who is not necessarily an operator |

The scope lives on the route ([`ui/src/routes.ts`](../ui/src/routes.ts)), so it
is one fact read three times: by the shell, which draws the switcher and the
scope's own navigation; by the route guard, which asks the policy table whether
this account may open the address; and by
[`design.test.ts`](../ui/src/lib/design.test.ts), which reads a screen's scope
off the same table rather than being told per file.

### What may be on a screen

**A Fleet- or Project-scope screen may not name a Kubernetes object.** Not a
Pod, not a Node, not a namespace, not a manifest, not a cluster Event, not a
PersistentVolume, PersistentVolumeClaim or StorageClass.
[docs/SCOPE.md](SCOPE.md) is the reason —

> The developer should never need the words "namespace" or "Deployment".

— and it is not about secrecy. The API decides what may be *read*, by role.
This decides what is *worth reading*, and a Kubernetes noun is the wrong answer
to every question those two scopes exist to ask. "Is my app up" is answered by
the health strip, the crash report and the findings; it is not answered better
by a pod name.

**A Platform- or Compliance-scope screen is about the cluster**, so it says all
of those words freely and nothing inside it is gated a second time.

### Why it is the screen and not the reader

There used to be a mode toggle — a header switch, derived from the platform
role, that rewrote the contents of six screens — and an `<OperatorOnly>`
element that wrapped the blocks it rewrote. Both are gone (#469), and the
reason is worth keeping:

- **A gate keyed to the reader makes a project's troubleshooting depend on who
  staffs it.** A project managed by somebody who also holds the operator role
  would get strictly better diagnostics than an identical project managed by a
  plain member. That is an accident of staffing becoming a product difference.
- **Half of what was gated was never operator content at all.**
  `status.conditions` was wrapped in four places — on a project, an
  environment, a build and an incident — so a developer whose build was broken
  could not read the single most diagnostic thing on the page. Conditions are a
  fact about the reader's own object and are shown to everybody.
- **The other half was a fact about the cluster on a developer's screen**, and
  the answer to that is not a gate but an address: the pod table, the
  materialized objects, the bound `PersistentVolume`, the destination namespace
  and the cluster's Warning events are the Platform scope's, where the reader
  who wants them is already going.
- **What is left is an instruction only an operator can act on** — "enable
  Hubble in Cilium and point…" on an empty traffic map. On a Project-scope
  screen that becomes a statement of fact: *no flow data reaches this project*.
  A developer is never told nothing, and never handed a button they cannot
  press.

So the line is what a thing *is*, not who is reading it, and it is checkable
from the route rather than from anybody remembering to write `v-if`.

### What the test checks

`design.test.ts` reads the rendered *words* — text nodes, and the attributes
that become text (`title`, `aria-label`, `placeholder`, `label`, `description`,
`empty`, `hint`, `alt`) — and refuses a Fleet- or Project-scope screen that
says one of the Kubernetes nouns. An expression like `pod.name` is not caught:
a field name is not a label, and what leaks is a screen *saying* Pod. A view
with no route at all is checked as though it were a developer's, which is the
strict reading — a screen nothing addresses cannot argue that its address
exempts it.

Two lists in that file are the escape hatches, and both are meant to be argued
with rather than added to quietly:

- `OPERATOR_COMPONENTS` — a component that is only ever mounted on a Platform-
  or Compliance-scope screen *and* speaks the vocabulary in its own text. It is
  deliberately the shortest list that works, three entries long, rather than
  every component such a screen happens to use: a component that is clean today
  stays checked, and so stays clean. Adding one means saying which screens
  mount it.
- `NOT_ABOUT_KUBERNETES` — the handful of phrases that contain one of the words
  and mean something else. Log *clustering* is the standing example.

### A screen that needs a project and was not given one

`/observability` is not a screen: it is a question about a project nobody
named. Such an address keeps the address it was asked for, renders the fleet
dashboard, and opens the project picker over it; choosing a project completes
the sentence and carries the query the link was asking with. The alternative —
guessing a project from a dropdown's last value — is what made a pasted
developer link mean something different for every reader.

### What the shell may remember

**Memory may decide a destination; it may never decide a rendering.**

The scope rule above turns on the address meaning one thing for every reader,
and the reason there is no project dropdown deciding what you see is written
into `routes.ts`: guessing a project from a dropdown's last value is what made
a pasted developer link mean something different for each person who opened it.

That rule is about what an address *renders*, and it is absolute. Where a
**control** points when nobody has named a project is a different question, and
answering it from memory costs the rule nothing: the address the control
produces is still explicit, still shareable, and still means exactly one thing.
So the shell remembers the last project screen somebody was on
([`lib/navigation.ts`](../ui/src/lib/navigation.ts)) and the scope switcher's
Project entry goes there, rather than asking again every time somebody comes
back from the Platform scope.

Three consequences, and each is the rule rather than a detail of it:

- **`/projects` still asks.** It is not redirected to the remembered project,
  because a redirect is the address meaning two things — precisely what the
  scope rule forbids. It keeps the picker, and the memory only stops you
  arriving there by accident.
- **What is remembered is a place, not a path**: a project and one of the six
  section screens. A screen naming an *object* — a build, an environment —
  degrades to the list it came from, since that object belongs to the project
  being left and would be a 404 or a stale row on return. The one query that
  survives is `?section=`, which names a pane of the destination screen; a log
  filter or a time range is a question about the project being left.
- **A remembered project that has gone is forgotten, not navigated to.** It is
  checked against the projects the account can currently see, so a deleted
  project — or one whose role was taken away — falls back to the picker.

It lives in `localStorage`, which is where anything of this kind belongs: a
per-viewer convenience that never reaches the API and never reaches another
reader. Both halves are wrapped, because a private window and a browser set to
block site data throw on the accessor itself rather than answering nothing —
so it falls back to an in-memory copy. The worst case is a switcher that has
forgotten, which is where the feature started.

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

## The overlay layer

Every overlay Nuxt UI opens — a dropdown menu, a select, a modal — is
teleported to the end of `<body>` and given no z-index of its own: it is meant
to win on document order alone. Under this shell it does not. The drawer is
`fixed z-50`, and a z-index beats document order however late the element
arrives, so the overlay is painted underneath the rail that opened it. The
failure says nothing — the menu opens, `aria-expanded` says so, the items are
laid out and focusable, and the screen looks like a control that does not
work. That is what the project switcher was.

So the order is named once, in
[`assets/main.css`](../ui/src/assets/main.css), and it has three layers and
no more:

| Layer | z-index | What is in it |
|---|---|---|
| The drawer's backdrop | 40 | `AppShell.vue`, below `lg` only |
| The rail | 50 | `AppShell.vue` |
| Everything portalled | 60 | The popper wrapper, and a modal's overlay and content |

Nuxt UI's toaster sits at 100 above all of them, which is right: a toast is
readable while a modal is open.

**A screen writes no z-index.** The two in `AppShell.vue` are the shell's
chrome and the only ones the dashboard has; anything a screen opens is in the
layer above them already. A third would be one screen deciding its own place
in an order the other twenty-two never stated, which is the drift this guide
exists to stop — `design.test.ts` holds this half.

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

### A screen with more sections than a scroll can hold

**Past about six sections, a screen gets a left rail and shows one pane at a
time.** Below that, sections on one scroll are how a page is read; above it they
are how a page becomes unreadable.

A project's Settings is the case that established the shape: fourteen panes —
source, processes, attached resources, variables, files, secrets, domains,
members, keys, notifications, runtime, security, continuity, danger zone — which
on one scroll were more lines of form than the whole of the page that held them
(#470). The Platform scope answers the same problem one level up, with a scope
and one screen at a time.

Four rules, and each of them is why this is written down rather than left to the
next screen to guess at:

- **The pane is in the address**, as `?section=`. A pane that is not addressable
  is a thing somebody has to describe over chat instead of linking to — and it
  is the same spelling a finding's evidence link already uses, so there is one
  vocabulary rather than two.
- **The rail is data, not markup.** The list of panes lives beside the screen
  (`SETTINGS_SECTIONS` in `ui/src/lib/project.ts`), because the redirects that
  land old addresses on the right pane read the same ids. Two spellings of one
  vocabulary is how a redirect quietly stops landing.
- **Each pane is `max-w-3xl`, declared once by the pane rather than by every
  panel inside it.** This is the "one page, one form width" rule above, honoured
  rather than asserted: the width is a property of the column, and a panel
  dropped into it inherits it.
- **A pane nobody may open is not in the rail**, and an address naming one falls
  back to the first pane this account has — the same rule every affordance here
  follows, asked of the same table the route guard asks.

A screen with a rail still has exactly one `PageHeader`: the rail is navigation
*within* a screen, not a second screen. `design.test.ts` checks that as it
checks every other page.

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

- **What counts as wrong is the API's `severity`, never the condition's
  status.** A `status` says whether the statement in a condition's type holds,
  and `Previews=False` with reason `Disabled` is previews turned off — a
  setting, drawn for a while as a red dot, a red condition line and a slot in
  this band. The API classifies every condition it serves
  ([API.md](API.md#conditions)), and `conditionSeverity` in `lib/status` is the
  only place the dashboard reads that from: anything deciding whether something
  is *wrong* — a tone, a failure count, a row hoisted up here — goes through it
  or through `unhealthyConditions` and `conditionsTone` above it, rather than
  comparing a status to `"True"`. Asking whether one named condition has
  arrived yet is a different question and stays a direct read. A list of benign
  reasons kept here would drift from the operator the first time somebody added
  one.

The `conditions` in the expanded row are a fact about the object that is
failing, so they are shown to everybody the row is shown to, like the error
line and the failing step's output beside them.

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

### A state that is neither good nor bad takes neither colour

A phase is drawn from `phaseTone` in
[`lib/status.ts`](../ui/src/lib/status.ts), and a phase that is not a fault and
not an achievement takes `info` or `neutral` there — never `error` or
`success`. The case that made the rule is a build the platform *skipped*: a
push whose source under the project's build root did not change is neither a
failure nor a deployment, and a monorepo's ordinary day is seven of them.
Painting them red would put seven healthy projects in the attention band;
painting them green would say seven services shipped. The row says what
happened in a `text-muted` line under the commit, the way a failure says it in
`text-error` and a stall in `text-warning`.

`lib/status.test.ts` holds this one rather than `design.test.ts`, which is the
frame and has no opinion about what a screen says.

## Identifiers, numbers and prose

`font-mono` for anything that is typed or copied: a name, a hostname, a SHA, a
URL, a duration, a count. Sentence case for everything a person reads. An em
dash — like this — rather than parentheses, which is the voice the rest of this
repository is written in and the dashboard is written in too.

### An identifier that links back to the source

A SHA, a branch name, a pull request number and a repository are linked with
[`SourceLink`](../ui/src/components/SourceLink.vue), and the dashboard composes
no URL of its own. The API serves `commitUrl`, `branchUrl`, `pullRequestUrl`
and `repositoryUrl` on the objects that carry the facts, because the host is a
fact about the project's connection rather than a constant — a GitLab or Gitea
connection can name a forge anybody self-hosted (#435).

`SourceLink` takes the URL it is given and renders a link when there is one and
the same text unchanged when there is not, which is the state every one of
these fields has: a build with no commit, a connection that is gone, a provider
with no web routing. A linked identifier takes the primary colour and an
unlinked one keeps the colour of the row it is in, so the colour is the
difference between "this goes somewhere" and "this is text".

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
`PageHeader` and `PageSection`, it declares its scope in `routes.ts`, and the
test tells you the one thing you forgot.
