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

package gitprovider

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

func providerConnection(providerName string, config string) *kitchenv1alpha1.Connection {
	conn := &kitchenv1alpha1.Connection{
		Spec: kitchenv1alpha1.ConnectionSpec{Provider: providerName},
	}
	if config != "" {
		conn.Spec.Config = &runtime.RawExtension{Raw: []byte(config)}
	}
	return conn
}

func TestDefaultResolvesGitProviders(t *testing.T) {
	cases := map[string]struct {
		conn   *kitchenv1alpha1.Connection
		token  string
		assert func(Provider) bool
	}{
		"github": {
			conn:  providerConnection("github", `{"apiUrl":"https://ghe.example.com/api/v3"}`),
			token: "gh",
			assert: func(p Provider) bool {
				gh, ok := p.(*GitHub)
				return ok && gh.APIURL == "https://ghe.example.com/api/v3" && gh.Token == "gh"
			},
		},
		"gitlab": {
			conn:  providerConnection("gitlab", `{"apiUrl":"https://gitlab.example.com/api/v4"}`),
			token: "gl",
			assert: func(p Provider) bool {
				gl, ok := p.(*GitLab)
				return ok && gl.APIURL == "https://gitlab.example.com/api/v4" && gl.Token == "gl"
			},
		},
		"gitea": {
			conn:  providerConnection("gitea", `{"apiUrl":"https://git.example.com/api/v1"}`),
			token: "gt",
			assert: func(p Provider) bool {
				gt, ok := p.(*Gitea)
				return ok && gt.APIURL == "https://git.example.com/api/v1" && gt.Token == "gt"
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := Default(tc.conn, tc.token)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.assert(p) {
				t.Fatalf("unexpected provider %#v", p)
			}
		})
	}
}

func TestDefaultFallsBackToTheHostedAPI(t *testing.T) {
	// A Connection naming no apiUrl is the hosted service. Every provider
	// here is one somebody can self-host, so this is the path a fumbled
	// default would break quietly.
	for provider, want := range map[string]string{
		"github": "https://api.github.com",
		"gitlab": "https://gitlab.com/api/v4",
		"gitea":  "https://gitea.com/api/v1",
	} {
		t.Run(provider, func(t *testing.T) {
			p, err := Default(providerConnection(provider, ""), "token")
			if err != nil {
				t.Fatal(err)
			}
			var got string
			switch impl := p.(type) {
			case *GitHub:
				got = impl.APIURL
			case *GitLab:
				got = impl.APIURL
			case *Gitea:
				got = impl.APIURL
			}
			if got != want {
				t.Errorf("api url is %q, want %q", got, want)
			}
		})
	}
}

func TestDefaultRejectsUnsupportedProviders(t *testing.T) {
	_, err := Default(providerConnection("svn", ""), "tok")
	if !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("expected ErrUnsupportedProvider, got %v", err)
	}
}
