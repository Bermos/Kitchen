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

package framework

import (
	"slices"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

func TestDetect(t *testing.T) {
	cases := map[string]struct {
		signals  Signals
		want     string
		wantNone bool
	}{
		"a Dockerfile wins over everything else in the repository": {
			signals: Signals{
				Dockerfile:  true,
				Files:       []string{"Dockerfile", "package.json", "go.mod"},
				PackageJSON: []byte(`{"dependencies":{"next":"15.0.0"}}`),
			},
			want: Dockerfile,
		},
		"next in the dependencies is Next.js": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"next":"15.0.0","react":"19.0.0"}}`),
			},
			want: NextJS,
		},
		"Nuxt is recognised before the Vite it brings with it": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"nuxt":"3.14.0"},"devDependencies":{"vite":"5.4.0"}}`),
			},
			want: Nuxt,
		},
		"so is SvelteKit": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"devDependencies":{"@sveltejs/kit":"2.8.0","vite":"5.4.0"}}`),
			},
			want: SvelteKit,
		},
		"Astro with the node adapter is a server": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"astro":"5.0.0","@astrojs/node":"9.0.0"}}`),
			},
			want: Astro,
		},
		"Astro without one is a directory of files": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"astro":"5.0.0"},"scripts":{"build":"astro build"}}`),
			},
			want: AstroStatic,
		},
		"Vite alone is a single-page application": {
			signals: Signals{
				Files:       []string{"package.json", "index.html"},
				PackageJSON: []byte(`{"devDependencies":{"vite":"5.4.0"},"scripts":{"build":"vite build"}}`),
			},
			want: Vite,
		},
		"a package.json with only a start script is a Node application": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"scripts":{"start":"node server.js"}}`),
			},
			want: Node,
		},
		"and so is one with a server.js and no scripts at all": {
			signals: Signals{
				Files:       []string{"package.json", "server.js"},
				PackageJSON: []byte(`{"dependencies":{"express":"4.21.0"}}`),
			},
			want: Node,
		},
		"a package.json that does not parse is still Node": {
			signals: Signals{
				Files:       []string{"package.json", "index.js"},
				PackageJSON: []byte(`{"dependencies":`),
			},
			want: Node,
		},
		"go.mod is Go": {
			signals: Signals{Files: []string{"go.mod", "main.go"}},
			want:    Go,
		},
		"pyproject.toml is Python": {
			signals: Signals{Files: []string{"pyproject.toml", "app"}},
			want:    Python,
		},
		"a Gemfile is Ruby": {
			signals: Signals{Files: []string{"Gemfile", "config.ru"}},
			want:    Ruby,
		},
		"pom.xml is Java": {
			signals: Signals{Files: []string{"pom.xml", "src"}},
			want:    Java,
		},
		"any .csproj is .NET": {
			signals: Signals{Files: []string{"Kitchen.Web.csproj", "Program.cs"}},
			want:    DotNet,
		},
		"an index.html and nothing else is a static site": {
			signals: Signals{Files: []string{"index.html", "style.css"}},
			want:    Static,
		},
		"a repository with none of the signals is not guessed at": {
			signals:  Signals{Files: []string{"README.md", "LICENSE"}},
			wantNone: true,
		},
		"neither is a package.json that builds into nothing runnable": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"scripts":{"test":"vitest"}}`),
			},
			wantNone: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := Detect(tc.signals)
			if ok == tc.wantNone {
				t.Fatalf("Detect() detected = %v, want %v", ok, !tc.wantNone)
			}
			if tc.wantNone {
				return
			}
			if got.Name != tc.want {
				t.Fatalf("Detect() = %q, want %q", got.Name, tc.want)
			}
			if got.Strategy == "" {
				t.Errorf("framework %q resolves to no build strategy", got.Name)
			}
		})
	}
}

func TestDetectBuildEnv(t *testing.T) {
	cases := map[string]struct {
		signals Signals
		want    map[string]string
	}{
		"a Vite app is built by its own script and served by NGINX": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"devDependencies":{"vite":"5.4.0"},"scripts":{"build":"vite build"}}`),
			},
			want: map[string]string{
				"BP_NODE_RUN_SCRIPTS":             "build",
				"BP_WEB_SERVER":                   "nginx",
				"BP_WEB_SERVER_ROOT":              "dist",
				"BP_WEB_SERVER_ENABLE_PUSH_STATE": "true",
			},
		},
		"create-react-app builds to build/ instead": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"react-scripts":"5.0.1"},"scripts":{"build":"react-scripts build"}}`),
			},
			want: map[string]string{
				"BP_NODE_RUN_SCRIPTS":             "build",
				"BP_WEB_SERVER":                   "nginx",
				"BP_WEB_SERVER_ROOT":              "build",
				"BP_WEB_SERVER_ENABLE_PUSH_STATE": "true",
			},
		},
		"a multi-page Astro site is served without push-state routing": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"astro":"5.0.0"},"scripts":{"build":"astro build"}}`),
			},
			want: map[string]string{
				"BP_NODE_RUN_SCRIPTS": "build",
				"BP_WEB_SERVER":       "nginx",
				"BP_WEB_SERVER_ROOT":  "dist",
			},
		},
		"a repository that is already the site has nothing to build": {
			signals: Signals{Files: []string{"index.html"}},
			want: map[string]string{
				"BP_WEB_SERVER":      "nginx",
				"BP_WEB_SERVER_ROOT": ".",
			},
		},
		// #468: the build script reached the three static frameworks and no
		// other, so a framework that compiles its own server was built with
		// an empty environment and never ran its build at all.
		"a framework that starts its own server is told to run its build": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"next":"15.0.0"},"scripts":{"build":"next build"}}`),
			},
			// Next.js starts with `npm start`, which is the manifest's own
			// script: `npm-start` fires on it, so there is no launch point
			// for the platform to name.
			want: map[string]string{"BP_NODE_RUN_SCRIPTS": "build"},
		},
		// #468: the runtime image carried no Node at all. node-engine marks
		// its layer for launch only where a start buildpack requires node at
		// launch, and a stock Nuxt manifest fires none of them — so the
		// platform names the file its own start command runs, and tells
		// node-start not to look for it before the build has written it.
		"nuxt is told to run its build, and where its server will be": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"nuxt":"3.14.0"},"scripts":{"build":"nuxt build"}}`),
			},
			want: map[string]string{
				"BP_NODE_RUN_SCRIPTS":   "build",
				"BP_LAUNCHPOINT":        ".output/server/index.mjs",
				"BP_VERIFY_LAUNCHPOINT": "false",
			},
		},
		"so are the other three frameworks that build their own server": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"@nestjs/core":"10.4.0"},"scripts":{"build":"nest build"}}`),
			},
			want: map[string]string{
				"BP_NODE_RUN_SCRIPTS":   "build",
				"BP_LAUNCHPOINT":        "dist/main",
				"BP_VERIFY_LAUNCHPOINT": "false",
			},
		},
		// The manifest is the only place a repository says which runtime it
		// wants. Without this the node-engine buildpack reports no version
		// source at all and takes whatever is newest that day.
		"a manifest that names a node version is passed it": {
			signals: Signals{
				Files: []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"nuxt":"3.14.0"},` +
					`"scripts":{"build":"nuxt build"},"engines":{"node":">=22.0.0"}}`),
			},
			want: map[string]string{
				"BP_NODE_RUN_SCRIPTS":   "build",
				"BP_NODE_VERSION":       ">=22.0.0",
				"BP_LAUNCHPOINT":        ".output/server/index.mjs",
				"BP_VERIFY_LAUNCHPOINT": "false",
			},
		},
		"a static framework is told both as well": {
			signals: Signals{
				Files: []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"vite":"5.0.0"},` +
					`"scripts":{"build":"vite build"},"engines":{"node":"22.x"}}`),
			},
			want: map[string]string{
				"BP_NODE_RUN_SCRIPTS":             "build",
				"BP_NODE_VERSION":                 "22.x",
				"BP_WEB_SERVER":                   "nginx",
				"BP_WEB_SERVER_ROOT":              "dist",
				"BP_WEB_SERVER_ENABLE_PUSH_STATE": "true",
			},
		},
		"a repository with no build script and no engine is told neither": {
			signals: Signals{
				Files:       []string{"package.json", "server.js"},
				PackageJSON: []byte(`{"scripts":{"start":"node server.js"}}`),
			},
			want: map[string]string{},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := Detect(tc.signals)
			if !ok {
				t.Fatalf("Detect() detected nothing")
			}
			env := map[string]string{}
			for _, v := range got.BuildEnv {
				env[v.Name] = v.Value
			}
			if len(env) != len(tc.want) {
				t.Fatalf("build env = %v, want %v", env, tc.want)
			}
			for name, want := range tc.want {
				if env[name] != want {
					t.Errorf("build env %s = %q, want %q", name, env[name], want)
				}
			}
			for i := 1; i < len(got.BuildEnv); i++ {
				if got.BuildEnv[i-1].Name > got.BuildEnv[i].Name {
					t.Fatalf("build env is not sorted: %v", got.BuildEnv)
				}
			}
		})
	}
}

// A framework whose build env is not attached to the catalogue entry it was
// copied from: two detections of the same framework must not share, or grow,
// one slice.
func TestDetectDoesNotMutateTheCatalogue(t *testing.T) {
	signals := Signals{
		Files:       []string{"package.json"},
		PackageJSON: []byte(`{"devDependencies":{"vite":"5.4.0"},"scripts":{"build":"vite build"}}`),
	}
	first, _ := Detect(signals)
	second, _ := Detect(signals)
	if len(first.BuildEnv) != len(second.BuildEnv) {
		t.Fatalf("second detection carried %d variables, first carried %d", len(second.BuildEnv), len(first.BuildEnv))
	}
	if entry := catalogue[Vite]; len(entry.BuildEnv) != 0 {
		t.Fatalf("the catalogue entry grew build variables: %v", entry.BuildEnv)
	}
}

func TestByName(t *testing.T) {
	f, ok := ByName(NextJS)
	if !ok {
		t.Fatalf("ByName(%q) found nothing", NextJS)
	}
	if f.Port != 3000 {
		t.Errorf("Next.js listens on %d, want 3000", f.Port)
	}
	if _, ok := ByName("something-a-newer-operator-detected"); ok {
		t.Error("ByName() resolved a framework this build does not know")
	}
}

// Every framework has to name a strategy, and every name in the catalogue has
// to be its own key: the name is what a Build records, and ByName is the only
// way back from it.
func TestCatalogueIsConsistent(t *testing.T) {
	for name, f := range catalogue {
		if f.Name != name {
			t.Errorf("catalogue entry %q is named %q", name, f.Name)
		}
		switch f.Strategy {
		case kitchenv1alpha1.BuildStrategyDockerfile, kitchenv1alpha1.BuildStrategyBuildpacks:
		default:
			t.Errorf("framework %q resolves to strategy %q", name, f.Strategy)
		}
	}
	if catalogue[Dockerfile].Port != 0 {
		t.Error("a Dockerfile decides its own port; detection must not imply one")
	}
}

// What each framework starts with, and which of them only start because the
// platform says so.
//
// The four commands here are the whole of #440's default path: their servers
// are written by the build into a directory that does not exist while the
// buildpacks are deciding what to run, so `npm-start`, `node-start` and the
// Procfile buildpack all fail to detect and the lifecycle exports an image
// declaring no process at all. The two that name `npm start` are the
// frameworks whose stock manifest binds that script to their own CLI.
func TestFrameworkCommands(t *testing.T) {
	for name, want := range map[string][]string{
		Nuxt:      {"node", ".output/server/index.mjs"},
		SvelteKit: {"node", "build"},
		NestJS:    {"node", "dist/main"},
		Astro:     {"node", "./dist/server/entry.mjs"},
		NextJS:    {"npm", "start"},
		Remix:     {"npm", "start"},

		// Plain Node is recognised from four shapes and the buildpacks read
		// the same signals; a command here would be the platform guessing
		// between them.
		Node: nil,

		// A Dockerfile declares its own entrypoint, the static frameworks are
		// started by the web-server buildpack, and every other language's
		// buildpack declares a process of its own.
		Dockerfile:  nil,
		Vite:        nil,
		ReactApp:    nil,
		AstroStatic: nil,
		Static:      nil,
		Go:          nil,
		Python:      nil,
		Ruby:        nil,
		Java:        nil,
		DotNet:      nil,
	} {
		f, ok := ByName(name)
		if !ok {
			t.Fatalf("ByName(%q) found nothing", name)
		}
		if !slices.Equal(f.Command, want) {
			t.Errorf("%s starts with %q, want %q", name, f.Command, want)
		}
	}
}

// Which frameworks the platform caps the build heap for. It is a fact about
// what runs the build, so the static front-ends are in it — a Vite bundle is
// assembled by the same tool a Nuxt server is, and dies the same way — and
// a directory that is already a website is not, because nothing builds it.
func TestFrameworksThatRunNode(t *testing.T) {
	node := map[string]bool{
		NextJS: true, Nuxt: true, SvelteKit: true, Remix: true, NestJS: true,
		Astro: true, AstroStatic: true, Vite: true, ReactApp: true, Node: true,
	}
	for name, f := range catalogue {
		if f.RunsNode != node[name] {
			t.Errorf("framework %q RunsNode = %v, want %v", name, f.RunsNode, node[name])
		}
	}
}

// The launch point, which is the same file the command names — and has to
// be, or the image is built to start one program and the platform starts
// another (#468).
//
// Only a `node <file>` command has one. `npm start` is the repository's own
// script, and the buildpack that reads it needs no help from the platform;
// a framework with no command at all is started by a buildpack that declares
// a process of its own.
func TestFrameworkLaunchPoints(t *testing.T) {
	want := map[string]string{
		Nuxt:      ".output/server/index.mjs",
		SvelteKit: "build",
		NestJS:    "dist/main",
		Astro:     "./dist/server/entry.mjs",
	}
	for name, f := range catalogue {
		got, ok := launchPoint(f.Command)
		if got != want[name] {
			t.Errorf("%s launches %q, want %q", name, got, want[name])
		}
		if ok != (want[name] != "") {
			t.Errorf("%s has a launch point = %v, want %v", name, ok, want[name] != "")
		}
		if !ok {
			continue
		}
		// And it is the command's own second word, not a copy of it that
		// could be edited on its own.
		if got != f.Command[1] {
			t.Errorf("%s starts %q and is built to start %q", name, f.Command[1], got)
		}
	}
}

// A launch point the platform names is always accompanied by the flag that
// stops node-start looking for it: the file is written by the build, so at
// detect time it is never there, and BP_LAUNCHPOINT alone fails the detect
// it was meant to pass.
func TestLaunchPointIsAlwaysUnverified(t *testing.T) {
	for name := range catalogue {
		env := map[string]string{}
		for _, v := range launchPointEnv(catalogue[name]) {
			env[v.Name] = v.Value
		}
		if _, named := env["BP_LAUNCHPOINT"]; !named {
			if len(env) != 0 {
				t.Errorf("%s is told %v without naming a launch point", name, env)
			}
			continue
		}
		if env["BP_VERIFY_LAUNCHPOINT"] != "false" {
			t.Errorf("%s names a launch point and verifies it: %v", name, env)
		}
	}
}

// A command that is not exactly `node <file>` is not a launch point: giving
// one to node-start would have it run `node <the whole command>`.
func TestLaunchPointRefusesEverythingElse(t *testing.T) {
	for name, command := range map[string][]string{
		"nothing at all":            nil,
		"a package manager script":  {"npm", "start"},
		"a bare program":            {"node"},
		"a program with flags":      {"node", "--enable-source-maps", "server.js"},
		"another runtime's program": {"python", "app.py"},
	} {
		t.Run(name, func(t *testing.T) {
			if file, ok := launchPoint(command); ok {
				t.Errorf("launchPoint(%q) = %q, want none", command, file)
			}
		})
	}
}

// Which tool locked the repository, from the lockfile at the build root.
//
// The lockfile is the signal rather than `packageManager` in the manifest,
// because Heroku's builder refuses to install without one: a field naming
// pnpm beside no pnpm-lock.yaml would send a repository to a builder that
// then turns it away.
func TestPackageManager(t *testing.T) {
	for name, tc := range map[string]struct {
		files []string
		want  PackageManager
	}{
		"no lockfile says nothing":   {files: []string{"package.json"}, want: PackageManagerUnknown},
		"package-lock.json is npm":   {files: []string{"package.json", "package-lock.json"}, want: NPM},
		"npm-shrinkwrap.json is too": {files: []string{"package.json", "npm-shrinkwrap.json"}, want: NPM},
		"yarn.lock is yarn":          {files: []string{"package.json", "yarn.lock"}, want: Yarn},
		"pnpm-lock.yaml is pnpm":     {files: []string{"package.json", "pnpm-lock.yaml"}, want: PNPM},
		"bun.lock is bun":            {files: []string{"package.json", "bun.lock"}, want: Bun},
		"so is the binary bun.lockb": {files: []string{"package.json", "bun.lockb"}, want: Bun},
		"two tools' lockfiles say nothing": {
			files: []string{"package.json", "package-lock.json", "pnpm-lock.yaml"},
			want:  PackageManagerUnknown,
		},
		"npm's own two lockfiles are still one answer": {
			files: []string{"package.json", "package-lock.json", "npm-shrinkwrap.json"},
			want:  NPM,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := packageManager(newFileSet(tc.files)); got != tc.want {
				t.Errorf("packageManager(%v) = %q, want %q", tc.files, got, tc.want)
			}
		})
	}
}

// Which builder each repository is built by, which is the whole of #568.
//
// A pnpm repository whose image starts a Node process of its own goes to
// Heroku's builder, which installs with the package manager the repository
// names. A pnpm repository whose image *serves a directory* cannot: the web
// server serving it is a Paketo buildpack with no equivalent there, so it
// stays where it is and the platform says so on the Build instead.
func TestDetectBuilder(t *testing.T) {
	const nuxt = `{"dependencies":{"nuxt":"3.14.0"},"scripts":{"build":"nuxt build"}}`
	const vite = `{"devDependencies":{"vite":"5.4.0"},"scripts":{"build":"vite build"}}`

	for name, tc := range map[string]struct {
		signals     Signals
		wantName    string
		wantBuilder Builder
		wantPM      PackageManager
		wantHonours bool
	}{
		"a pnpm Nuxt repository is built by Heroku's builder": {
			signals:     Signals{Files: []string{"package.json", "pnpm-lock.yaml"}, PackageJSON: []byte(nuxt)},
			wantName:    Nuxt,
			wantBuilder: BuilderHeroku,
			wantPM:      PNPM,
			wantHonours: true,
		},
		"an npm Nuxt repository is not moved": {
			signals:     Signals{Files: []string{"package.json", "package-lock.json"}, PackageJSON: []byte(nuxt)},
			wantName:    Nuxt,
			wantBuilder: BuilderPaketo,
			wantPM:      NPM,
			wantHonours: true,
		},
		"nor is a yarn one": {
			signals:     Signals{Files: []string{"package.json", "yarn.lock"}, PackageJSON: []byte(nuxt)},
			wantName:    Nuxt,
			wantBuilder: BuilderPaketo,
			wantPM:      Yarn,
			wantHonours: true,
		},
		"nor one that locked nothing at all": {
			signals:     Signals{Files: []string{"package.json"}, PackageJSON: []byte(nuxt)},
			wantName:    Nuxt,
			wantBuilder: BuilderPaketo,
			wantPM:      PackageManagerUnknown,
			wantHonours: true,
		},
		"a pnpm front-end stays on the builder that can serve it": {
			signals:     Signals{Files: []string{"package.json", "pnpm-lock.yaml"}, PackageJSON: []byte(vite)},
			wantName:    Vite,
			wantBuilder: BuilderPaketo,
			wantPM:      PNPM,
			wantHonours: false,
		},
		// Plain Node names no command of its own and leans on the image
		// declaring a process. Heroku's buildpack reads the same signals
		// Paketo's start buildpacks do — a start script, or an entry file —
		// which are the shapes detection recognises it by, so moving it does
		// not produce an image with nothing to start.
		"a pnpm repository with only an entry file still moves": {
			signals: Signals{
				Files:       []string{"package.json", "server.js", "pnpm-lock.yaml"},
				PackageJSON: []byte(`{"dependencies":{"express":"4.21.2"}}`),
			},
			wantName:    Node,
			wantBuilder: BuilderHeroku,
			wantPM:      PNPM,
			wantHonours: true,
		},
		"a bun repository has nowhere to go": {
			signals:     Signals{Files: []string{"package.json", "bun.lockb"}, PackageJSON: []byte(nuxt)},
			wantName:    Nuxt,
			wantBuilder: BuilderPaketo,
			wantPM:      Bun,
			wantHonours: false,
		},
		"a language with no lockfile of this kind is untouched": {
			signals:     Signals{Files: []string{"go.mod"}},
			wantName:    Go,
			wantBuilder: BuilderPaketo,
			wantPM:      PackageManagerUnknown,
			wantHonours: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := Detect(tc.signals)
			if !ok {
				t.Fatalf("Detect(%v) recognised nothing", tc.signals.Files)
			}
			if got.Name != tc.wantName {
				t.Fatalf("detected %q, want %q", got.Name, tc.wantName)
			}
			if got.Builder != tc.wantBuilder {
				t.Errorf("builder is %q, want %q", got.Builder, tc.wantBuilder)
			}
			if got.PackageManager != tc.wantPM {
				t.Errorf("package manager is %q, want %q", got.PackageManager, tc.wantPM)
			}
			if got.HonoursLockfile() != tc.wantHonours {
				t.Errorf("HonoursLockfile() = %v, want %v", got.HonoursLockfile(), tc.wantHonours)
			}
		})
	}
}

// Heroku's builder is told nothing, and that is deliberate rather than an
// omission: it reads package.json for the package manager, the Node version
// and the build script, and it keeps the Node runtime for launch whether or
// not anything asked — so every BP_* name the Paketo path sets would be a
// variable no buildpack in that builder reads.
func TestHerokuBuildsAreConfiguredByTheRepository(t *testing.T) {
	manifest := []byte(`{"dependencies":{"nuxt":"3.14.0"},` +
		`"scripts":{"build":"nuxt build"},"engines":{"node":"22.x"}}`)

	paketo, _ := Detect(Signals{
		Files:       []string{"package.json", "package-lock.json"},
		PackageJSON: manifest,
	})
	if len(paketo.BuildEnv) == 0 {
		t.Fatal("the Paketo path must still tell its buildpacks what to build")
	}

	heroku, _ := Detect(Signals{
		Files:       []string{"package.json", "pnpm-lock.yaml"},
		PackageJSON: manifest,
	})
	if heroku.Builder != BuilderHeroku {
		t.Fatalf("builder is %q, want %q", heroku.Builder, BuilderHeroku)
	}
	if len(heroku.BuildEnv) != 0 {
		t.Errorf("Heroku's builder was handed %v, and reads none of it", heroku.BuildEnv)
	}
	// The heap cap is the platform's own and reaches every Node build
	// whatever builds it, so this must stay true of the moved ones.
	if !heroku.RunsNode {
		t.Error("a pnpm Nuxt build still runs under Node, and still needs its heap capped")
	}
	// The name, the port and the command are the framework's identity and
	// are read back from a Release long after the build: moving the builder
	// must not move any of them.
	if heroku.Name != paketo.Name || heroku.Port != paketo.Port ||
		!slices.Equal(heroku.Command, paketo.Command) {
		t.Errorf("the builder changed the framework's identity: %+v vs %+v", heroku, paketo)
	}
}
