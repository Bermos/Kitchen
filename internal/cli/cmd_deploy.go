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
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// `kitchen deploy` — build this commit, and follow what the platform does with
// it.
//
// The interesting half is the following, and it is a renderer for something the
// API already does: build logs stream over Server-Sent Events, phases are a
// poll of the build, and a release and an environment are two more objects
// that appear when they appear. Nothing here drives the platform through a
// deploy — the platform does that on its own the moment the Build exists.
//
// It emits the same events either way it is watched. A person gets them drawn
// under a spinner; a pipe gets one JSON object per line, ending in a `result`.
// That is the rule the whole CLI follows: one description of what happened,
// two ways of showing it.

// How the follow paces itself. The build is polled rather than watched because
// the API has no watch, and two seconds is what its own log stream polls at —
// a phase and the lines that explain it should not be a tick apart.
//
// They are variables rather than constants only so the tests can turn a
// five-second wait into a five-millisecond one; nothing outside a test changes
// them.
var (
	buildPollInterval = 2 * time.Second
	// logGrace is how long the log stream is left open after the build has
	// stopped. The collector ships a line after the container wrote it, so the
	// last few lines of a build arrive after the phase has already moved.
	logGrace = 3 * time.Second
	// releaseWait bounds the look for the Release a successful build makes.
	// The reconciler writes it as soon as it sees the build succeed.
	releaseWait = 2 * time.Minute
	// degradedSettle is how long an environment has to read Degraded before
	// the deploy is called failed.
	//
	// Degraded is not by itself a verdict: an environment carries the phase
	// its *last* pass left on it, so a retry of a failed migration reads
	// Degraded for the moment between the release being promoted onto it and
	// the reconciler looking at the new one. Stopping at the first Degraded —
	// which is what this command used to do — would report that as a failed
	// deploy. Waiting a few polls costs a genuine failure this much and
	// nothing else, because a failed deploy stays failed: nothing about it
	// changes until somebody builds, rolls back or retries.
	degradedSettle = 15 * time.Second
)

// The event types a followed deploy emits, spelled once. They are the `type`
// field a caller reading the stream switches on, so they are constants rather
// than literals scattered over the renderer.
const (
	eventBuild       = "build"
	eventLog         = "log"
	eventRelease     = "release"
	eventEnvironment = "environment"
	eventResult      = "result"
)

// deployEvent is one thing that happened, in the shape --json publishes.
type deployEvent struct {
	// Type is one of the five constants above.
	Type        string       `json:"type"`
	Build       *build       `json:"build,omitempty"`
	Line        *logLine     `json:"line,omitempty"`
	Release     *release     `json:"release,omitempty"`
	Environment *environment `json:"environment,omitempty"`
	// URL is where the deployed environment answers, on the result event.
	URL string `json:"url,omitempty"`
	// OK is on the result event alone: whether the deploy worked — the build
	// succeeded and, where one was waited for, the environment did not settle
	// Degraded. It is the same fact the exit status carries, for a caller
	// reading one line of JSON rather than a status.
	OK *bool `json:"ok,omitempty"`
}

func newDeployCommand(r *Runtime) *cobra.Command {
	var (
		sha       string
		branch    string
		detach    bool
		rebuild   bool
		noLogs    bool
		noWait    bool
		waitLimit time.Duration
	)

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Build the current commit and follow the deploy",
		Long: strings.TrimSpace(`
Build a commit of the linked project and follow what the platform does with it.

With no flags it builds the commit this working copy is on, which has to be a
commit the platform's git provider can see: the build clones it, so a commit
that has not been pushed cannot be built. --sha and --branch say so explicitly,
which is how this runs somewhere that is not a checkout at all.

Following prints the build's own output as it arrives and then reports the
release and the environment that picked it up. --detach starts the build and
prints it; --no-wait stops when the build does.

The exit status is the deploy's: 0 when it worked, 9 when the build failed or
was cancelled, and 12 when the build succeeded and the environment settled
Degraded — the release was refused, and what was serving before it still is.
An environment that has not gone live within --environment-timeout is neither:
"not live yet" is in the result, because it is a different question from
whether the deploy worked.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			return deploy(commandContext(cmd), r, deployOptions{
				sha: sha, branch: branch, detach: detach, rebuild: rebuild,
				noLogs: noLogs, noWait: noWait, environmentWait: waitLimit,
			})
		}),
	}

	flags := cmd.Flags()
	flags.StringVar(&sha, "sha", "", "the commit to build. The default is this working copy's HEAD")
	flags.StringVar(&branch, "branch", "",
		"the branch that commit is on. The default is this working copy's, and the project's production branch for a commit the platform has not seen")
	flags.BoolVarP(&detach, "detach", "d", false, "start the build and print it, without following")
	flags.BoolVar(&rebuild, "rebuild", false,
		"rebuild whatever the project built last, ignoring the working copy — a rerun after a flake or a changed secret")
	flags.BoolVar(&noLogs, "no-logs", false, "follow the phases without streaming the build's output")
	flags.BoolVar(&noWait, "no-wait", false, "stop when the build does, without waiting for the environment")
	flags.DurationVar(&waitLimit, "environment-timeout", 5*time.Minute,
		"how long to wait for the environment to go live before answering with whatever phase it is in")

	return describe(cmd, meta{
		Calls: []string{
			"POST /api/v1/projects/{name}/builds",
			"GET /api/v1/builds/{name}",
			"GET /api/v1/builds/{name}/logs",
			"GET /api/v1/projects/{name}/releases",
			"GET /api/v1/projects/{name}/environments",
		},
		Output: output{
			Mode: outputStream, Kind: "deployEvent",
			Note: "one JSON object per line: build, log, release and environment events, ending in a result event. " +
				"--detach answers a single build document instead",
		},
		Needs: needs{Auth: true, Project: true, Git: true},
		Examples: []example{
			{"Deploy this commit and follow it as JSON events",
				"kitchen deploy --json --timeout 30m"},
			{"Deploy a commit from somewhere that is not a checkout",
				"kitchen deploy --project shop --sha 1a2b3c4 --branch main --json"},
			{"Start a build and stop, printing the build",
				"kitchen deploy --detach --json"},
			{"Rerun the last build of the project",
				"kitchen deploy --rebuild --json"},
		},
	})
}

func newCancelCommand(r *Runtime) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "cancel [BUILD]",
		Short: "Stop a build that is still running",
		Long: strings.TrimSpace(`
Cancel a build.

With no build named it cancels the project's newest one that has not finished.
The build itself stays, in phase Cancelled and with who cancelled it recorded:
builds are the history of who asked for what, so cancelling one never removes
it. A build that already finished is a conflict rather than a no-op.`),
		Args: cobra.MaximumNArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return cancel(commandContext(cmd), r, name, yes)
		}),
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask first")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/projects/{name}/builds", "POST /api/v1/builds/{name}/cancel"},
		Output: output{Mode: outputDocument, Kind: "build"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{"Stop whatever is building now", "kitchen cancel --yes --json"},
			{"Stop a particular build", "kitchen cancel shop-bld-abc123def456-xk2p9 --yes --json"},
		},
	})
}

func cancel(parent context.Context, r *Runtime, name string, yes bool) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	ctx, cancelContext := r.context(parent)
	defer cancelContext()

	if name == "" {
		project, err := r.projectName()
		if err != nil {
			return err
		}
		builds, err := client.projectBuilds(ctx, project)
		if err != nil {
			return err
		}
		for i := range builds {
			if !builds[i].terminal() {
				name = builds[i].Name
				break
			}
		}
		if name == "" {
			return failf(codeNotFound, "nothing is building on %s", project).
				withHint("`kitchen builds` lists them; a finished build cannot be cancelled")
		}
	}

	if err := confirm(r, "Cancel "+name+"?", yes); err != nil {
		return err
	}
	cancelled, err := client.cancelBuild(ctx, name)
	if err != nil {
		return err
	}
	return r.printer().document(cancelled, func(s tui.Styles) string {
		return fmt.Sprintf("%s %s %s\n", s.OK.Render("Cancelled"),
			s.Title.Render(cancelled.Name), s.Phase(cancelled.Phase))
	})
}

// deployOptions is what the flags said, kept as one value so the function that
// does the work has one parameter rather than seven.
type deployOptions struct {
	sha             string
	branch          string
	detach          bool
	rebuild         bool
	noLogs          bool
	noWait          bool
	environmentWait time.Duration
}

func deploy(parent context.Context, r *Runtime, options deployOptions) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	name, err := r.projectName()
	if err != nil {
		return err
	}
	ctx, cancel := r.context(parent)
	defer cancel()

	sha, branch, err := commitToBuild(ctx, r, options)
	if err != nil {
		return err
	}

	started, err := client.startBuild(ctx, name, sha, branch)
	if err != nil {
		return err
	}

	if options.detach {
		return r.printer().document(started, func(s tui.Styles) string {
			return fmt.Sprintf("%s %s  %s\n%s\n",
				s.OK.Render("Started"), s.Title.Render(started.Name), s.Phase(started.Phase),
				s.Subtle.Render("follow it with: kitchen logs --build "+started.Name+" --follow"))
		})
	}

	d := &deployment{runtime: r, client: client, project: name, options: options}
	return d.follow(ctx, started)
}

// commitToBuild decides what is being built: the flags, and otherwise what the
// working copy says.
//
// The two warnings it raises are the two ways a deploy surprises somebody: a
// dirty tree means the build will not contain what is on the screen, and an
// unpushed commit means it cannot be built at all. Neither refuses — the
// platform's answer is the authoritative one, and a CLI that second-guesses it
// is a CLI somebody has to argue with.
func commitToBuild(ctx context.Context, r *Runtime, options deployOptions) (string, string, error) {
	if options.rebuild {
		if options.sha != "" {
			return "", "", fail(codeUsage, "--rebuild and --sha ask for different builds").
				withHint("--rebuild builds whatever the project built last; --sha names a commit")
		}
		// An empty body is what the API reads as "again".
		return "", "", nil
	}
	if options.sha != "" {
		return options.sha, options.branch, nil
	}

	revision := describeRevision(ctx, r.WorkingDir)
	if revision.SHA == "" {
		return "", "", fail(codeUsage, "not a git working copy, so there is no commit to build").
			withHint("pass --sha <commit> (and --branch), or --rebuild to build the project's last commit again")
	}
	if revision.Dirty {
		r.printer().warn("the working copy has uncommitted changes: the platform builds the commit, not the files here")
	}
	if !revision.Pushed {
		r.printer().warn("%s is not on any remote branch: the build clones from the git provider, so push it first",
			short(revision.SHA))
	}
	branch := options.branch
	if branch == "" {
		branch = revision.Branch
	}
	return revision.SHA, branch, nil
}

// deployment is one followed build.
type deployment struct {
	runtime *Runtime
	client  *client
	project string
	options deployOptions

	// emit is guarded because the log stream and the phase poll are two
	// goroutines describing one deploy.
	mutex sync.Mutex
	send  func(deployEvent)
}

// follow watches a build to its end and, when it succeeded, the release and
// environment that follow from it.
func (d *deployment) follow(ctx context.Context, started *build) error {
	printer := d.runtime.printer()

	if !d.runtime.interactive() {
		d.send = func(event deployEvent) {
			_ = printer.event(event, func(s tui.Styles) string { return renderDeployEvent(s, event) })
		}
		return d.watch(ctx, started)
	}

	// A person is watching: the same events, drawn under a spinner. The work
	// runs in a goroutine because the Bubble Tea program owns this one until
	// there is nothing left to draw.
	events := make(chan tui.Event, 256)
	styles := printer.styles
	// Set before the goroutine starts, not inside it: emit is called from two
	// other goroutines, and assigning it after they may already be running
	// would be a race whichever way it went.
	d.send = func(event deployEvent) {
		line := renderDeployEvent(styles, event)
		select {
		case events <- tui.Event{Line: line, Phase: phaseOf(event)}:
		case <-ctx.Done():
		}
	}

	var watchErr error
	go func() {
		defer close(events)
		watchErr = d.watch(ctx, started)
	}()

	title := d.project + " " + started.Name
	if err := tui.Follow(d.runtime.Stdin, d.runtime.Stderr, styles, title, events); err != nil {
		// The terminal could not be drawn on. The build is still running and
		// still worth reporting, so this is a note rather than the answer.
		printer.note("%v", err)
	}
	return watchErr
}

// phaseOf is what the status block should say after this event.
func phaseOf(event deployEvent) string {
	switch {
	case event.Build != nil && event.Build.Phase != "":
		return event.Build.Phase
	case event.Type == eventRelease:
		return "releasing"
	case event.Type == eventEnvironment:
		return "deploying"
	default:
		return ""
	}
}

// watch is the work: stream the logs, poll the phase, and then follow what the
// platform makes of a successful build.
func (d *deployment) watch(ctx context.Context, started *build) error {
	d.emit(deployEvent{Type: eventBuild, Build: started})

	logs, stopLogs := context.WithCancel(ctx)
	defer stopLogs()
	var streaming sync.WaitGroup
	if !d.options.noLogs {
		streaming.Add(1)
		go func() {
			defer streaming.Done()
			d.streamLogs(logs, started.Name)
		}()
	}

	finished, err := d.awaitBuild(ctx, started)
	if err != nil {
		return err
	}

	// Give the collector a moment to ship the last lines: it tails a file the
	// container wrote, so the end of a build's output arrives after its phase
	// has already moved.
	if !d.options.noLogs {
		select {
		case <-time.After(logGrace):
		case <-ctx.Done():
		}
	}
	stopLogs()
	streaming.Wait()

	result := deployEvent{Type: eventResult, Build: finished}
	built := finished.Phase == phaseSucceeded

	// The deploy's verdict, which is the build's until there is an
	// environment to have one of its own.
	var refused error
	if built && !d.options.noWait {
		released, deployed, err := d.awaitDeploy(ctx, finished)
		result.Release, result.Environment, refused = released, deployed, err
		if deployed != nil {
			result.URL = deployed.URL
		}
	}

	ok := built && refused == nil
	result.OK = &ok
	d.emit(result)

	if !built {
		return failf(codeBuildFailed, "the build %s ended %s", finished.Name, strings.ToLower(finished.Phase)).
			withHint("its output is above; `kitchen logs --build " + finished.Name + "` reads it again")
	}
	return refused
}

// awaitBuild polls until the build stops moving, reporting every phase change.
func (d *deployment) awaitBuild(ctx context.Context, started *build) (*build, error) {
	current := started
	if current.terminal() {
		return current, nil
	}

	ticker := time.NewTicker(buildPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, interrupted(ctx, "the build "+started.Name+" is still running")
		case <-ticker.C:
		}

		latest, err := d.client.build(ctx, started.Name)
		if err != nil {
			return nil, err
		}
		// The stall is a change worth reporting even though the phase has
		// not moved: a follow that says "Running" and nothing else for ten
		// minutes is exactly the experience the reconciler's Stalled
		// condition exists to end.
		if latest.Phase != current.Phase || latest.stalledReason() != current.stalledReason() {
			d.emit(deployEvent{Type: eventBuild, Build: latest})
		}
		current = latest
		if current.terminal() {
			return current, nil
		}
	}
}

// streamLogs tails the build's output. A platform with no telemetry store has
// no logs to give, which is a 503 and a note rather than a failed deploy: the
// build is running either way, and refusing to follow it would be a worse
// answer than following it quietly.
func (d *deployment) streamLogs(ctx context.Context, name string) {
	err := d.client.followLogs(ctx, "/builds/"+name+"/logs", nil, func(line logLine) error {
		d.emit(deployEvent{Type: eventLog, Line: &line})
		return nil
	})
	switch {
	case err == nil, ctx.Err() != nil:
		return
	default:
		d.runtime.printer().note("not following the build's output: %v", asFailure(err).Error())
	}
}

// awaitDeploy follows what a successful build turns into: the Release the
// reconciler writes from it, and the Environment that picks the release up.
//
// Neither *arriving* is an error. A build of a branch nothing deploys produces
// a release nothing promotes, and an environment that is slow to go live is a
// fact about the environment rather than about this command — both are
// reported as what was actually seen, and both exit 0.
//
// An environment that settles Degraded is the one thing here that is a
// failure, and it is the reason this returns an error at all: the platform
// refused the release and went on serving the previous one, which a caller
// reading only the exit status has to be told about. Settling is the whole of
// the judgement — see degradedSettle.
func (d *deployment) awaitDeploy(ctx context.Context, finished *build) (*release, *environment, error) {
	released := d.awaitRelease(ctx, finished.Name)
	if released == nil {
		return nil, nil, nil
	}
	d.emit(deployEvent{Type: eventRelease, Release: released})

	var last *environment
	// degradedSince is when the environment first read Degraded with nothing
	// on it saying work was still in flight. Zero whenever it is anything
	// else, so a Degraded that clears starts the count again.
	var degradedSince time.Time
	deadline := time.Now().Add(d.options.environmentWait)
	for {
		environments, err := d.client.projectEnvironments(ctx, d.project)
		if err != nil {
			d.runtime.printer().note("could not read the environments: %v", asFailure(err).Error())
			return released, last, nil
		}
		for i := range environments {
			if environments[i].Release != released.Name {
				continue
			}
			current := environments[i]
			if last == nil || current.Phase != last.Phase || current.URL != last.URL {
				d.emit(deployEvent{Type: eventEnvironment, Environment: &current})
			}
			last = &current

			switch {
			case current.Phase == phaseLive:
				return released, last, nil
			case current.Phase != phaseDegraded || current.deployInFlight():
				degradedSince = time.Time{}
			case degradedSince.IsZero():
				degradedSince = time.Now()
			case time.Since(degradedSince) >= degradedSettle:
				return released, last, degradedFailure(last)
			}
		}
		if !time.Now().After(deadline) && sleep(ctx, buildPollInterval) {
			continue
		}
		// The wait is over. An environment that read Degraded for the whole
		// of it is the same refused deploy, reported late rather than not at
		// all — unless the wait ended because somebody interrupted it, which
		// is not a verdict on anything.
		if ctx.Err() == nil && !degradedSince.IsZero() {
			return released, last, degradedFailure(last)
		}
		return released, last, nil
	}
}

// degradedFailure is what a settled Degraded exits with. The reason is the
// environment's own condition, which for the case this exists for — a deploy
// task that failed — is the sentence the reconciler wrote naming the task, its
// run and what the run said.
func degradedFailure(env *environment) error {
	message := env.Name + " ended degraded: the release did not take traffic"
	if why := env.degradedReason(); why != "" {
		message = env.Name + " ended degraded: " + why
	}
	return fail(codeDeployFailed, message).
		withHint("what was serving before this release still is; `kitchen processes --environment " +
			env.Name + "` shows the run and `kitchen logs --environment " + env.Name + "` reads its output")
}

// awaitRelease looks for the release a build produced.
func (d *deployment) awaitRelease(ctx context.Context, buildName string) *release {
	deadline := time.Now().Add(releaseWait)
	for {
		releases, err := d.client.projectReleases(ctx, d.project)
		if err != nil {
			d.runtime.printer().note("could not read the releases: %v", asFailure(err).Error())
			return nil
		}
		for i := range releases {
			if releases[i].Build == buildName {
				return &releases[i]
			}
		}
		if time.Now().After(deadline) || !sleep(ctx, buildPollInterval) {
			return nil
		}
	}
}

// emit publishes one event, once, to whichever renderer is watching.
func (d *deployment) emit(event deployEvent) {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	if d.send != nil {
		d.send(event)
	}
}

// renderDeployEvent is one event as a person reads it.
func renderDeployEvent(s tui.Styles, event deployEvent) string {
	switch event.Type {
	case eventLog:
		return renderLogLine(s, *event.Line, false)
	case eventBuild:
		b := event.Build
		line := fmt.Sprintf("%s %s %s %s", s.Accent.Render("build"), b.Name,
			s.Phase(b.Phase), s.Subtle.Render(short(b.Git.SHA)+" "+b.Git.Branch))
		if reason := b.stalledReason(); reason != "" {
			line += "\n" + s.Bad.Render("stalled: "+reason)
		}
		return line
	case eventRelease:
		return fmt.Sprintf("%s %s %s", s.Accent.Render("release"), event.Release.Name,
			s.Subtle.Render(event.Release.Image))
	case eventEnvironment:
		return fmt.Sprintf("%s %s %s %s", s.Accent.Render("environment"), event.Environment.Name,
			s.Phase(event.Environment.Phase), s.Subtle.Render(event.Environment.URL))
	case eventResult:
		return renderDeployResult(s, event)
	default:
		return ""
	}
}

func renderDeployResult(s tui.Styles, event deployEvent) string {
	// The build's own line is drawn from the build's phase rather than from
	// `ok`, which is the whole deploy's: a build that succeeded into an
	// environment that refused the release is `ok: false` and still a build
	// that succeeded.
	if event.Build == nil {
		return ""
	}
	if event.Build.Phase != phaseSucceeded {
		return s.Bad.Render("✗ build " + strings.ToLower(event.Build.Phase))
	}
	built := s.OK.Render("✓ build succeeded")
	// Why it took as long as it did, where the platform knows: a build with
	// nothing to reuse is slow for a reason, and one that reads as a
	// regression is the thing this line exists to prevent.
	if cache := event.Build.Cache; cache != nil && cache.Enabled {
		if cache.Warm {
			built += " " + s.Subtle.Render("cache warm")
		} else {
			built += " " + s.Subtle.Render("cache cold — the next build reuses these layers")
		}
	}
	lines := []string{built}
	if event.Environment != nil {
		where := event.Environment.Name
		if event.URL != "" {
			where += " " + s.Accent.Render(event.URL)
		}
		mark := s.OK.Render("✓")
		if event.Environment.Phase == phaseDegraded {
			mark = s.Bad.Render("✗")
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", mark, where, s.Phase(event.Environment.Phase)))
	}
	return strings.Join(lines, "\n")
}

// sleep waits, and reports whether it got to the end rather than being
// interrupted.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// interrupted is what a wait that was cut short answers with. It distinguishes
// the two ways that happens, because they mean different things to whoever is
// reading the status: a timeout is the CLI giving up, an interrupt is somebody
// asking it to.
func interrupted(ctx context.Context, still string) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fail(codeTimedOut, "gave up waiting").withHint(still + "; raise --timeout to wait longer")
	}
	return fail(codeInterrupted, "interrupted").withHint(still)
}

// queryOf builds a query string from pairs, leaving out the empty ones so an
// unset flag is an absent parameter rather than an empty one the API has to
// interpret.
func queryOf(pairs ...[2]string) url.Values {
	query := url.Values{}
	for _, pair := range pairs {
		if pair[1] != "" {
			query.Set(pair[0], pair[1])
		}
	}
	return query
}
