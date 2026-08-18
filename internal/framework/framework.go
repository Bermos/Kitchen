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

	// BuildEnv is what the builder has to be told. It is empty for every
	// framework that starts a server of its own, and carries the web-server
	// configuration for the ones that build to a directory of files.
	BuildEnv []BuildVar
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

// catalogue is every framework by name: the strategy that builds it and the
// port it serves on. It is the lookup ByName answers from, so a Build whose
// status says "vite" still resolves to a port a release later, with nothing
// re-read from the repository.
//
// BuildEnv is deliberately absent here and computed in Detect instead: the
// only variable that depends on the repository rather than on the framework
// is whether there is a build script to run, and a map of slices that
// callers append to is a way to hand out shared backing arrays.
var catalogue = map[string]Framework{
	Dockerfile: {Name: Dockerfile, Strategy: kitchenv1alpha1.BuildStrategyDockerfile},

	NextJS:    {Name: NextJS, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000},
	Nuxt:      {Name: Nuxt, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000},
	SvelteKit: {Name: SvelteKit, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000},
	Remix:     {Name: Remix, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000},
	NestJS:    {Name: NestJS, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000},
	Astro:     {Name: Astro, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 4321},
	Node:      {Name: Node, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000},

	Go:     {Name: Go, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080},
	Python: {Name: Python, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8000},
	Ruby:   {Name: Ruby, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 3000},
	Java:   {Name: Java, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080},
	DotNet: {Name: DotNet, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080},

	// The static ones are served by NGINX, which the buildpack configures to
	// listen on $PORT; 8080 is only what the platform then sets PORT to.
	Vite:        {Name: Vite, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080},
	ReactApp:    {Name: ReactApp, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080},
	AstroStatic: {Name: AstroStatic, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080},
	Static:      {Name: Static, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks, Port: 8080},
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

	// Frontend builds run the project's own build script; the platform names
	// it rather than inferring an output directory from nothing.
	build := []BuildVar(nil)
	if pkg.Scripts["build"] != "" {
		build = []BuildVar{{Name: "BP_NODE_RUN_SCRIPTS", Value: "build"}}
	}

	switch {
	case deps["next"]:
		return catalogue[NextJS], true
	case deps["nuxt"], deps["nuxt3"]:
		return catalogue[Nuxt], true
	case deps["@sveltejs/kit"]:
		return catalogue[SvelteKit], true
	case deps["@remix-run/serve"], deps["@remix-run/node"]:
		return catalogue[Remix], true
	case deps["@nestjs/core"]:
		return catalogue[NestJS], true
	case deps["astro"]:
		// Astro is a static site generator until an adapter makes it a
		// server, and the adapter is the only thing in the repository that
		// says which of the two this is.
		if deps["@astrojs/node"] {
			return catalogue[Astro], true
		}
		return withEnv(catalogue[AstroStatic], build, nginx("dist", false)), true
	case deps["react-scripts"]:
		return withEnv(catalogue[ReactApp], build, nginx("build", true)), true
	case deps["vite"]:
		// Vite with none of the frameworks above is a single-page
		// application: built to dist/, served as files, and routed entirely
		// in the browser — which is what push-state is for.
		return withEnv(catalogue[Vite], build, nginx("dist", true)), true
	case pkg.Scripts["start"] != "", files.has("server.js"), files.has("index.js"), files.has("app.js"):
		return catalogue[Node], true
	}
	// A package.json with no start script and no recognised framework builds
	// into nothing anyone can run, and saying so is the point of the
	// feature.
	return Framework{}, false
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
