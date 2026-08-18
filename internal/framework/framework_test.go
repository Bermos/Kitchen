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
		"a framework that starts its own server is told nothing": {
			signals: Signals{
				Files:       []string{"package.json"},
				PackageJSON: []byte(`{"dependencies":{"next":"15.0.0"},"scripts":{"build":"next build"}}`),
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
