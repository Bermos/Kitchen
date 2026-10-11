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

// Command registry-token-server runs the operator's registry token service
// on a fixed set of accounts, for hack/check-registry-accepts-docker-manifests.sh.
//
// That check starts the zot the chart deploys on the configuration the chart
// renders, and zot authenticates nobody itself: it sends every client to the
// token service. So the check needs one, and this is the same handler the
// operator serves — internal/registrytoken — with the accounts given on the
// command line instead of read out of a cluster. What is under test is the
// pairing: that the key the chart generates signs tokens the registry the
// chart configures accepts, and that a project's token reaches that project's
// repositories and no other's.
//
// Each -account is `username:password:grant`, where grant is `platform`,
// `read`, or `project=<name>` / `project-pull=<name>`.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Bermos/Kitchen/internal/registrytoken"
)

type accountFlags []string

func (a *accountFlags) String() string     { return strings.Join(*a, ",") }
func (a *accountFlags) Set(v string) error { *a = append(*a, v); return nil }

func main() {
	listen := flag.String("listen", "127.0.0.1:8093", "address to serve on")
	keyFile := flag.String("key", "", "PEM file holding the signing key (the chart's tokenKey)")
	path := flag.String("path", "/kitchen/token", "path the token endpoint is served at")
	var accounts accountFlags
	flag.Var(&accounts, "account", "username:password:grant, repeatable")
	flag.Parse()

	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		log.Fatal(err)
	}
	key, err := registrytoken.ParseKey(raw)
	if err != nil {
		log.Fatal(err)
	}
	static := &registrytoken.StaticAccounts{Key: key, Accounts: map[string]registrytoken.StaticAccount{}}
	for _, spec := range accounts {
		username, password, grant, err := parseAccount(spec)
		if err != nil {
			log.Fatal(err)
		}
		static.Accounts[username] = registrytoken.StaticAccount{Password: password, Grant: grant}
	}

	mux := http.NewServeMux()
	mux.Handle(*path, &registrytoken.Handler{Accounts: static})
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func parseAccount(spec string) (string, string, registrytoken.Grant, error) {
	username, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return "", "", registrytoken.Grant{}, fmt.Errorf("account %q: want username:password:grant", spec)
	}
	separator := strings.LastIndex(rest, ":")
	if separator < 0 {
		return "", "", registrytoken.Grant{}, fmt.Errorf("account %q: want username:password:grant", spec)
	}
	password, grant := rest[:separator], rest[separator+1:]
	switch {
	case grant == "platform":
		return username, password, registrytoken.PlatformGrant(), nil
	case grant == "read":
		return username, password, registrytoken.ReadGrant(), nil
	case strings.HasPrefix(grant, "project="):
		return username, password, registrytoken.ProjectGrant(strings.TrimPrefix(grant, "project="), true), nil
	case strings.HasPrefix(grant, "project-pull="):
		return username, password, registrytoken.ProjectGrant(strings.TrimPrefix(grant, "project-pull="), false), nil
	}
	return "", "", registrytoken.Grant{}, fmt.Errorf("account %q: unknown grant %q", spec, grant)
}
