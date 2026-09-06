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
	// configuration for a framework that builds to a directory of files, and
	// for every Node framework the build script to run and the runtime
	// version to run it under.
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
// BuildEnv is deliberately absent here and computed in Detect instead,
// because every variable in it depends on the repository rather than on the
// framework — whether there is a build script to run, and which runtime
// version the manifest asks for — and a map of slices that callers append to
// is a way to hand out shared backing arrays.
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

	switch {
	case deps["next"]:
		return withEnv(catalogue[NextJS], node), true
	case deps["nuxt"], deps["nuxt3"]:
		return withEnv(catalogue[Nuxt], node), true
	case deps["@sveltejs/kit"]:
		return withEnv(catalogue[SvelteKit], node), true
	case deps["@remix-run/serve"], deps["@remix-run/node"]:
		return withEnv(catalogue[Remix], node), true
	case deps["@nestjs/core"]:
		return withEnv(catalogue[NestJS], node), true
	case deps["astro"]:
		// Astro is a static site generator until an adapter makes it a
		// server, and the adapter is the only thing in the repository that
		// says which of the two this is.
		if deps["@astrojs/node"] {
			return withEnv(catalogue[Astro], node), true
		}
		return withEnv(catalogue[AstroStatic], node, nginx("dist", false)), true
	case deps["react-scripts"]:
		return withEnv(catalogue[ReactApp], node, nginx("build", true)), true
	case deps["vite"]:
		// Vite with none of the frameworks above is a single-page
		// application: built to dist/, served as files, and routed entirely
		// in the browser — which is what push-state is for.
		return withEnv(catalogue[Vite], node, nginx("dist", true)), true
	case pkg.Scripts["start"] != "", files.has("server.js"), files.has("index.js"), files.has("app.js"):
		return withEnv(catalogue[Node], node), true
	}
	// A package.json with no start script and no recognised framework builds
	// into nothing anyone can run, and saying so is the point of the
	// feature.
	return Framework{}, false
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
func withEnv(f Framework, groups ...[]BuildVar) Framework {
	env := []BuildVar{}
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
