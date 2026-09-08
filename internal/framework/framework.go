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

// Package framework recognises what a repository is from the files at its
// build root, and answers the three questions a zero-config deploy has to
// answer without being told: how to build it, what the builder needs to know
// about it, and which port the result listens on.
//
// It knows nothing about Kubernetes or about git hosting: the caller collects
// the signals and applies the verdict. That is what makes the rules — the
// half of this feature people will argue about — a table with a test next to
// it rather than something buried in a reconciler.
package framework

import (
	"encoding/json"
	"path"
	"slices"
	"sort"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// BuildVar is one variable the builder is told about the repository before it
// starts. The Cloud Native Buildpacks lifecycle takes its whole configuration
// this way: BP_WEB_SERVER is how a directory of static files becomes an
// image that serves them, and there is no other channel to say so.
type BuildVar struct {
	Name  string
	Value string
}

// PackageManager is what locked a JavaScript repository's dependencies, read
// from the lockfile at the build root.
//
// It matters because a lockfile is a promise about the versions an
// application was tested against, and only a builder carrying a buildpack for
// the tool that wrote one can keep it. Handed a repository whose lockfile it
// cannot read, Paketo's `npm-install` resolves `package.json` again from
// scratch and installs whatever is newest — a build that is not reproducible
// and ships versions nobody tried — or, on a tree npm's own resolver cannot
// read, dies inside npm with a message about neither npm nor pnpm (#568).
type PackageManager string

const (
	// PackageManagerUnknown is a build root with no lockfile at all, one
	// carrying several tools' lockfiles — where nothing has been said and the
	// platform builds the repository the way it always has — and every
	// repository that is not a JavaScript one.
	PackageManagerUnknown PackageManager = ""

	NPM  PackageManager = "npm"
	Yarn PackageManager = "yarn"
	PNPM PackageManager = "pnpm"
	Bun  PackageManager = "bun"
)

// Builder is which Cloud Native Buildpacks builder builds a repository.
//
// There are two, and the second exists for one reason: Paketo's builders
// carry no pnpm buildpack — not the pinned one, not the newest one, not the
// "full" one — so a pnpm repository built by one is built by npm against a
// lockfile npm never wrote. Heroku's builder selects the package manager from
// the repository itself and installs with it, which is the whole of the fix.
//
// It is a build-time fact like BuildEnv, not part of a framework's identity:
// ByName resolves a Release long after the build and answers neither this nor
// PackageManager, because neither is read again after the image exists.
type Builder string

const (
	// BuilderPaketo is the platform's default builder and the zero value.
	// It builds every language the platform recognises, and every framework
	// whose image serves a directory of files rather than starting a process
	// of its own — those are served by a Paketo web-server buildpack that has
	// no equivalent anywhere else.
	BuilderPaketo Builder = ""

	// BuilderHeroku builds a Node repository that Paketo's builder cannot:
	// one whose dependencies are locked by pnpm.
	BuilderHeroku Builder = "heroku"
)

// Framework is one thing a repository can be recognised as.
//
// Name is what lands in Build.status.detectedFramework, and every name is
// distinct — "astro" and "astro-static" are two entries rather than one,
// because the same source builds into two different images depending on
// whether an adapter is configured, and a reader of the build page should be
// able to tell which of the two happened.
type Framework struct {
	// Name identifies the framework, and is what the platform records and
	// shows. It is stable: it is written into a Build's status and read back
	// by ByName long after the detection ran.
	Name string

	// Strategy is how an image is made from this repository. Everything but
	// a Dockerfile build is Cloud Native Buildpacks — the decision issue #116
	// settled.
	Strategy kitchenv1alpha1.BuildStrategy

	// Port the built image listens on, and the value the platform sets PORT
	// to. Zero means the framework does not imply one: a Dockerfile decides
	// its own port, and nothing here should overrule it.
	//
	// For a buildpacks-built image the number is close to arbitrary — every
	// buildpack's answer to "which port" is $PORT, which the platform sets
	// from this — but it is the framework's own conventional port so that an
	// application which ignores PORT and hardcodes the convention still
	// works.
	Port int32

	// BuildEnv is what the builder has to be told: the web-server
	// configuration for a framework that builds to a directory of files, for
	// every Node framework the build script to run and the runtime version to
	// run it under, and for a framework that starts its own server the file
	// that server is built into — without which the image is exported with no
	// Node runtime in it (see launchPointEnv).
	BuildEnv []BuildVar

	// Command is how the framework's own documentation starts what it built,
	// and what the platform runs when the project names nothing itself. It
	// is the sibling of Port and is defaulted the same way — an explicit
	// `spec.runtime.command` wins over it, and a Release freezes whichever
	// one it took.
	//
	// It exists because a Cloud Native Buildpacks image is not guaranteed to
	// declare anything to start. A start process comes from one of three
	// optional buildpacks — a `start` script, a `server.js` or a `main` that
	// exists, or a Procfile — and a framework that builds its server into a
	// directory that does not exist until after the build satisfies none of
	// them. The lifecycle then exports an image with `processes: []`, which
	// is a build that succeeds and a workload that cannot run (#440).
	//
	// It is empty wherever the start is the repository's own business rather
	// than the framework's: a Dockerfile declares its entrypoint, the
	// non-Node languages' buildpacks all declare a process, and the static
	// frameworks are started by the web-server buildpack.
	//
	// Where it is a `node <file>` command it is also what the *image* is
	// built to start, because the same file becomes BP_LAUNCHPOINT — see
	// launchPointEnv, which is what puts a Node runtime in the image at all.
	Command []string

	// RunsNode says the repository's build runs under Node.js. It is a fact
	// about the *build* rather than about what it builds into, so it is true
	// of the static front-ends as well as of the servers — a Vite bundle is
	// assembled by the same `npm run build` a Nuxt server is.
	//
	// The platform reads it to cap the build's heap. V8 sizes its old space
	// from the machine it thinks it is on rather than from the cgroup it is
	// in, so a Node build under a memory limit will happily grow past it and
	// be killed — which arrives as exit 137 and no explanation at all. See
	// buildHeapMiB in the controller.
	RunsNode bool

	// PackageManager is what locked this repository's dependencies, empty
	// where nothing did. Like BuildEnv it is a fact about the repository the
	// build reads rather than part of the framework's identity, so ByName
	// does not answer it.
	PackageManager PackageManager

	// Builder is which Cloud Native Buildpacks builder builds this
	// repository, and is the zero value — Paketo's — for all but the case
	// Paketo cannot build. It is on the framework rather than worked out in
	// the reconciler because it is decided by the same reading of the same
	// directory that decides everything else here, and because the builder
	// and what the builder is told have to be chosen together: BuildEnv is
	// the Paketo `BP_*` vocabulary, which means nothing to any other builder.
	Builder Builder
}

// HonoursLockfile reports whether the builder that will build this repository
// carries a buildpack for the tool that locked it — that is, whether the
// versions the application was tested against are the versions it will be
// built with.
//
// False is not a refusal. The build still runs, and usually still succeeds:
// what it produces is an image whose dependencies were resolved from
// `package.json` afresh rather than taken from the lockfile beside it. That
// is worth saying out loud on the Build, which is why this is a question with
// an answer rather than an error — see noteLockfile in the controller.
func (f Framework) HonoursLockfile() bool {
	switch f.PackageManager {
	case PackageManagerUnknown, NPM, Yarn:
		// Both builders install with either, and a repository that locked
		// nothing has nothing to be honoured.
		return true
	case PNPM:
		return f.Builder == BuilderHeroku
	}
	// Bun locks with neither, and no builder the platform has carries a bun
	// buildpack: there is nowhere to send it.
	return false
}

// Names of the frameworks the platform recognises. They are exported because
// they are a public vocabulary: the value in a Build's status, and what a
// project's owner reads on the build page.
const (
	// Dockerfile is not a framework at all — it is the repository saying it
	// does not need one, and it wins over every other signal.
	Dockerfile = "dockerfile"

	NextJS      = "nextjs"
	Nuxt        = "nuxt"
	SvelteKit   = "sveltekit"
	Remix       = "remix"
	NestJS      = "nestjs"
	Astro       = "astro"
	AstroStatic = "astro-static"
	Vite        = "vite"
	ReactApp    = "create-react-app"
	Node        = "node"
	Go          = "go"
	Python      = "python"
	Ruby        = "ruby"
	Java        = "java"
	DotNet      = "dotnet"
	Static      = "static"
)

// catalogue is every framework by name: the strategy that builds it, the port
// it serves on, and how it is started. It is the lookup ByName answers from,
// so a Build whose status says "vite" still resolves to a port a release
// later, with nothing re-read from the repository — which is also why Command
// is here rather than computed in Detect: the Release is cut from the name
// alone.
//
// BuildEnv is deliberately absent here and assembled in Detect instead. Most
// of it depends on the repository rather than on the framework — whether
// there is a build script to run, and which runtime version the manifest asks
// for — and the part that does not (the launch point, derived from Command)
// belongs on the same slice: a map of slices that callers append to is a way
// to hand out shared backing arrays.
var catalogue = map[string]Framework{
	Dockerfile: {Name: Dockerfile, Strategy: kitchenv1alpha1.BuildStrategyDockerfile},

	// The Node servers. Each Command is the framework's own documented way to
	// start what it just built, cited beside it; the ones that name `npm
	// start` are the frameworks whose stock manifest binds that script to
	// their CLI, which is also the argv the `npm-start` buildpack would have
	// generated — so naming it changes nothing where that buildpack fires and
	// supplies the process type where it does not.
	//
	// nextjs.org/docs/app/getting-started/deploying — `next build`, then
	// `next start`, which the stock manifest's `start` script runs.
	NextJS: {Name: NextJS, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000,
		RunsNode: true, Command: []string{"npm", "start"}},
	// nuxt.com/docs/getting-started/deployment — "node .output/server/index.mjs".
	// Nitro always writes the server there, and stock Nuxt has no `start`
	// script at all, which is the whole of #440: nothing in the repository
	// tells any buildpack how to start it.
	Nuxt: {Name: Nuxt, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000,
		RunsNode: true, Command: []string{"node", ".output/server/index.mjs"}},
	// svelte.dev/docs/kit/adapter-node — "node build". Stock SvelteKit has no
	// `start` script either.
	SvelteKit: {Name: SvelteKit, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000,
		RunsNode: true, Command: []string{"node", "build"}},
	// remix.run/docs — the stock template's `start` script is
	// `remix-serve ./build/server/index.js`, and the path it names moved
	// between major versions where the script did not.
	Remix: {Name: Remix, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000,
		RunsNode: true, Command: []string{"npm", "start"}},
	// docs.nestjs.com/deployment — the stock `start:prod` script is
	// `node dist/main`, which is the Nest CLI's default output. `npm start`
	// is deliberately not it: that script is `nest start`, which compiles.
	NestJS: {Name: NestJS, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000,
		RunsNode: true, Command: []string{"node", "dist/main"}},
	// docs.astro.build/en/guides/integrations-guide/node — "node
	// ./dist/server/entry.mjs", the Node adapter's own default output.
	Astro: {Name: Astro, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 4321,
		RunsNode: true, Command: []string{"node", "./dist/server/entry.mjs"}},
	// Plain Node names no command on purpose. It is recognised from four
	// different shapes — a `start` script, a `server.js`, an `index.js`, an
	// `app.js` — and the buildpacks that turn those into a process type read
	// the same signals: `npm-start` takes the script, `node-start` takes
	// `server.js` or a `main` that exists. A command written here would have
	// to guess between them, and guessing wrong would replace a build that
	// refuses with a sentence (NoDefaultProcessType) with a pod that
	// crash-loops. The repository is the only thing that knows.
	Node: {Name: Node, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000,
		RunsNode: true},

	Go:     {Name: Go, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080},
	Python: {Name: Python, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8000},
	Ruby:   {Name: Ruby, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000},
	Java:   {Name: Java, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080},
	DotNet: {Name: DotNet, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080},

	// The static ones are served by NGINX, which the buildpack configures to
	// listen on $PORT; 8080 is only what the platform then sets PORT to. They
	// need no command for the same reason — the web-server buildpack declares
	// the process — but three of the four are still assembled by Node, and
	// pay the same heap tax while they are.
	Vite: {Name: Vite, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080,
		RunsNode: true},
	ReactApp: {Name: ReactApp, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080,
		RunsNode: true},
	AstroStatic: {Name: AstroStatic, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080,
		RunsNode: true},
	Static: {Name: Static, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080},
}

// ByName resolves a framework the platform recorded earlier. The second
// return is false for a name this build of the operator does not know, which
// is what a Build detected by a newer one looks like: callers keep whatever
// they already have rather than treating it as an error.
func ByName(name string) (Framework, bool) {
	f, ok := catalogue[name]
	return f, ok
}

// Signals is what a repository looks like at the directory a build builds.
// The caller reads them; nothing here goes to the network.
type Signals struct {
	// Dockerfile says the build's configured Dockerfile is there. It is a
	// field of its own rather than a name in Files because which file that
	// is, is the project's to decide.
	Dockerfile bool

	// Files are the entry names — files and directories alike — directly in
	// the build's root directory. Nothing recurses: a framework that cannot
	// be recognised from the top of its own directory is not one the
	// platform should be guessing at.
	Files []string

	// PackageJSON is the file's contents when there is one, and nil
	// otherwise. Unparseable JSON is read as "a Node project that says
	// nothing", never as an error: a repository is free to be broken, and
	// the build is where that gets reported.
	PackageJSON []byte
}

// packageJSON is the part of the manifest detection reads.
type packageJSON struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Scripts         map[string]string `json:"scripts"`
	Engines         map[string]string `json:"engines"`
}

// Detect recognises the repository, or reports that it cannot. The second
// return is false only when nothing matched at all — the case that has to
// reach the user as a sentence rather than as a builder's stack trace.
//
// A Dockerfile wins over everything. That is the least surprising rule: it is
// what a repository with one already got before detection existed, and it is
// the escape hatch for every repository detection reads wrongly.
func Detect(s Signals) (Framework, bool) {
	if s.Dockerfile {
		return catalogue[Dockerfile], true
	}

	files := newFileSet(s.Files)

	if s.PackageJSON != nil {
		return detectNode(s.PackageJSON, files)
	}

	switch {
	case files.has("go.mod"):
		return catalogue[Go], true
	case files.has("requirements.txt"), files.has("pyproject.toml"), files.has("Pipfile"):
		return catalogue[Python], true
	case files.has("Gemfile"):
		return catalogue[Ruby], true
	case files.has("pom.xml"), files.has("build.gradle"), files.has("build.gradle.kts"):
		return catalogue[Java], true
	case files.hasSuffix(".csproj"), files.hasSuffix(".sln"):
		return catalogue[DotNet], true
	case files.has("index.html"):
		// A repository that is already the site: no build step, and the
		// files are served from where they are.
		return withEnv(catalogue[Static], nginx(".", false)), true
	}
	return Framework{}, false
}

// detectNode reads the package manifest, which is the only place a JavaScript
// repository says what it is. The order is specific before general: every
// framework here depends on the ones below it — Nuxt, SvelteKit and Astro all
// bring Vite with them — so the first match has to be the most particular one.
func detectNode(manifest []byte, files fileSet) (Framework, bool) {
	pkg := packageJSON{}
	// A manifest that does not parse still says "Node": the buildpack will
	// have its own opinion about it, and reporting "no framework detected"
	// for a repository that plainly has a package.json would be a worse lie
	// than reporting the language.
	_ = json.Unmarshal(manifest, &pkg)

	deps := map[string]bool{}
	for name := range pkg.Dependencies {
		deps[name] = true
	}
	for name := range pkg.DevDependencies {
		deps[name] = true
	}

	// What the Node buildpacks have to be told about this repository. It is
	// the same two things whatever the framework turns out to be, and every
	// Node framework gets it — the servers as much as the static ones, which
	// is the bug #468 opened on: BP_NODE_RUN_SCRIPTS reached three
	// frameworks out of ten, so a stock Nuxt or Next repository was built
	// with an empty environment and its own `build` script never ran.
	node := nodeEnv(pkg)

	// Which tool wrote the lockfile beside the manifest, which is what
	// decides whether Paketo's builder can install this repository's
	// dependencies at all.
	pm := packageManager(files)

	// A framework whose image starts a Node process of its own can be built
	// by either builder, so it is built by whichever keeps the repository's
	// lockfile.
	//
	// Heroku's builder is told nothing: it reads `package.json` itself for
	// all three things Paketo has to be handed — which package manager to
	// install with, which Node version to install, and that there is a
	// `build` script to run — and it keeps the Node runtime for launch
	// unconditionally, so the BP_LAUNCHPOINT dance #440 needed there is not
	// needed here either. Sending it the `BP_*` names would be neither read
	// nor true.
	//
	// Plain Node is the one entry here that names no command of its own and
	// relies on the image declaring a process — so what makes it safe to move
	// is that Heroku's buildpack reads the same signals Paketo's start
	// buildpacks do: a `start` script, and failing that an entry file
	// (`server.js`, `index.js`, `main`), which are the shapes detection
	// recognises it by. A builder that stopped doing so would turn that case
	// into an image with `processes: []` and nothing to fall back on.
	server := func(f Framework) (Framework, bool) {
		f.PackageManager = pm
		if pm == PNPM {
			f.Builder = BuilderHeroku
			return f, true
		}
		return withEnv(f, node), true
	}
	// A framework whose image serves a directory of files is built by
	// Paketo's builder whatever locked it, because the web server serving
	// that directory *is* a Paketo buildpack: there is no BP_WEB_SERVER
	// anywhere else, and an image built without it has nothing to start. So
	// a pnpm front-end is still installed by npm — the platform says so on
	// the Build rather than quietly resolving it (see HonoursLockfile).
	served := func(f Framework, web []BuildVar) (Framework, bool) {
		f.PackageManager = pm
		return withEnv(f, node, web), true
	}

	switch {
	case deps["next"]:
		return server(catalogue[NextJS])
	case deps["nuxt"], deps["nuxt3"]:
		return server(catalogue[Nuxt])
	case deps["@sveltejs/kit"]:
		return server(catalogue[SvelteKit])
	case deps["@remix-run/serve"], deps["@remix-run/node"]:
		return server(catalogue[Remix])
	case deps["@nestjs/core"]:
		return server(catalogue[NestJS])
	case deps["astro"]:
		// Astro is a static site generator until an adapter makes it a
		// server, and the adapter is the only thing in the repository that
		// says which of the two this is.
		if deps["@astrojs/node"] {
			return server(catalogue[Astro])
		}
		return served(catalogue[AstroStatic], nginx("dist", false))
	case deps["react-scripts"]:
		return served(catalogue[ReactApp], nginx("build", true))
	case deps["vite"]:
		// Vite with none of the frameworks above is a single-page
		// application: built to dist/, served as files, and routed entirely
		// in the browser — which is what push-state is for.
		return served(catalogue[Vite], nginx("dist", true))
	case pkg.Scripts["start"] != "", files.has("server.js"), files.has("index.js"), files.has("app.js"):
		return server(catalogue[Node])
	}
	// A package.json with no start script and no recognised framework builds
	// into nothing anyone can run, and saying so is the point of the
	// feature.
	return Framework{}, false
}

// packageManager reads the lockfile at the build root, which is the only
// thing in a JavaScript repository that says which tool installs it and is
// worth believing. `packageManager` in the manifest is a version pin the
// builders read for themselves and is not enough on its own: Heroku's builder
// requires a lockfile to install at all, so a field naming pnpm beside no
// `pnpm-lock.yaml` would send a repository to a builder that then refuses it.
//
// Nothing recurses. A pnpm workspace locks at the repository root and a build
// root inside it has no lockfile of its own, which reads as "nothing said"
// here — the same answer detection gives to every other question it can only
// see one directory's worth of.
//
// Several tools' lockfiles at once is also "nothing said" rather than a
// winner picked here. The repository has not settled the question, and the
// answer the platform can defend is the one it has always given: build it the
// way it was built yesterday.
func packageManager(files fileSet) PackageManager {
	found := []PackageManager(nil)
	for _, lock := range []struct {
		file string
		pm   PackageManager
	}{
		{"package-lock.json", NPM},
		{"npm-shrinkwrap.json", NPM},
		{"yarn.lock", Yarn},
		{"pnpm-lock.yaml", PNPM},
		{"bun.lock", Bun},
		{"bun.lockb", Bun},
	} {
		if files.has(lock.file) && !slices.Contains(found, lock.pm) {
			found = append(found, lock.pm)
		}
	}
	if len(found) != 1 {
		return PackageManagerUnknown
	}
	return found[0]
}

// nodeEnv is what the Node buildpacks are told about a repository: which
// script builds it, and which runtime version to build and run it under.
//
// Both are things only the manifest knows, and both are silent when they are
// missing. Without BP_NODE_RUN_SCRIPTS the `node-run-script` buildpack runs
// nothing, so a framework that compiles itself ships an image with no build
// output in it. Without BP_NODE_VERSION the node-engine buildpack reports no
// version source at all —
//
//	Candidate version sources (in priority order):
//	            -> ""
//	Selected Node Engine version (using ): 24.18.1
//
// — and the application silently gets whatever is newest that day, which is a
// runtime upgrade nobody asked for and nothing recorded.
//
// The constraint is passed through exactly as `engines.node` wrote it:
// BP_NODE_VERSION takes a semver range, which is what that field already is.
func nodeEnv(pkg packageJSON) []BuildVar {
	vars := []BuildVar(nil)
	if pkg.Scripts["build"] != "" {
		vars = append(vars, BuildVar{Name: "BP_NODE_RUN_SCRIPTS", Value: "build"})
	}
	if version := strings.TrimSpace(pkg.Engines["node"]); version != "" {
		vars = append(vars, BuildVar{Name: "BP_NODE_VERSION", Value: version})
	}
	return vars
}

// launchPointEnv is how a framework that builds its own server ends up in an
// image that can run it.
//
// A Node runtime is a buildpack layer rather than part of the base image, and
// the node-engine buildpack marks that layer for *launch* only when something
// in the build plan requires `node` with `launch: true`. Only the start
// buildpacks do: `npm-start` requires it where the manifest has a `start`
// script, `node-start` where it can find the application's entry file. A
// framework that writes its server into a directory the build has not
// produced yet satisfies neither, so the group passes on node-engine,
// npm-install and node-run-script — all three of which want Node at build
// time only — and the exported image contains no Node at all. The platform
// hands the framework's command to the launcher correctly and it still dies:
//
//	node: line 1: node: command not found
//
// BP_LAUNCHPOINT names the file node-start should start, and
// BP_VERIFY_LAUNCHPOINT is that buildpack's own answer to a launch point
// "that is generated and may not exist yet": with it set to false, detect
// stops stat-ing the path and passes, which is the whole of the fix. Neither
// asks anything of the repository. What follows is the layer that carries
// `node`, the launch `node_modules` the deploy tasks and workers need to
// resolve their imports, and a default process type the image did not have.
//
// It is derived from Command rather than declared beside it so that the two
// cannot disagree: the file the image is built to start is the file the
// platform starts. A command that is not exactly `node <file>` gets neither
// variable — `npm start` is the manifest's own script, which is what makes
// `npm-start` fire for the two frameworks that name it.
func launchPointEnv(f Framework) []BuildVar {
	file, ok := launchPoint(f.Command)
	if !ok {
		return nil
	}
	return []BuildVar{
		{Name: "BP_LAUNCHPOINT", Value: file},
		{Name: "BP_VERIFY_LAUNCHPOINT", Value: "false"},
	}
}

// launchPoint is the file a start command runs, for the commands that name
// one: `node <file>`, and nothing else. A command with flags in it, or one
// that runs a package manager, is not a launch point — node-start would build
// `node <the whole thing>` out of it, which is a different program from the
// one the framework documented.
func launchPoint(command []string) (string, bool) {
	if len(command) == 2 && command[0] == "node" {
		return command[1], true
	}
	return "", false
}

// nginx is the web-server buildpack's configuration for a directory of static
// files: it generates an nginx.conf serving root, listening on $PORT.
//
// pushState is for applications that route in the browser — every path has to
// answer with index.html, or a reload of anything but "/" is a 404 the
// application never sees.
func nginx(root string, pushState bool) []BuildVar {
	vars := []BuildVar{
		{Name: "BP_WEB_SERVER", Value: "nginx"},
		{Name: "BP_WEB_SERVER_ROOT", Value: root},
	}
	if pushState {
		vars = append(vars, BuildVar{Name: "BP_WEB_SERVER_ENABLE_PUSH_STATE", Value: "true"})
	}
	return vars
}

// withEnv copies a catalogue entry with build variables attached, sorted by
// name so the same repository always produces the same pod spec — an
// unordered environment would rewrite the Job on every reconcile.
//
// What the framework itself implies is added here rather than at each call:
// a start command the repository declares nothing about has to reach the
// lifecycle as well, or the image is built without the runtime that command
// needs. See launchPointEnv.
func withEnv(f Framework, groups ...[]BuildVar) Framework {
	env := append([]BuildVar{}, launchPointEnv(f)...)
	for _, group := range groups {
		env = append(env, group...)
	}
	sort.Slice(env, func(i, j int) bool { return env[i].Name < env[j].Name })
	f.BuildEnv = env
	return f
}

// fileSet is the build root's listing, looked up by name.
type fileSet map[string]bool

func newFileSet(names []string) fileSet {
	set := make(fileSet, len(names))
	for _, name := range names {
		set[path.Base(name)] = true
	}
	return set
}

func (f fileSet) has(name string) bool { return f[name] }

// hasSuffix is for the languages that name their project file after the
// project — .NET's, which is <anything>.csproj.
func (f fileSet) hasSuffix(suffix string) bool {
	for name := range f {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}
