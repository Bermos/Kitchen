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

package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// External is a server somebody else runs, reached over the URL its
// Connection holds: Upstash, ElastiCache, Aiven, or the Valkey a team
// already has.
//
// It provisions nothing. What it does is give each claim a keyspace of its
// own at that server — a logical database number — and refuse what the
// server cannot honour.
//
// **That refusal is the whole of this provider's design.** A database number
// is not an isolation boundary and this package says so at length, which is
// why the in-cluster provider does not use one; here there is no choice,
// because nobody can ask somebody else's Redis for another process. So the
// honesty has to go somewhere else: the operator states on the Connection
// what the server is configured for, and a claim asking for something else
// is refused rather than bound to a server that will not behave the way the
// claim assumed. A claim that would be handed an evicting server for a queue
// never binds.
type External struct {
	// URL of the server, from the Connection's credentials Secret.
	URL string
	// Usage the operator declared the server is configured for. Empty means
	// they did not say, and a claim naming one is refused: guessing is what
	// this contract exists to prevent.
	Usage Usage
	// Databases is how many logical databases the server offers, so that a
	// claim beyond the last one is refused rather than bound to a keyspace
	// the server will reject on first use.
	Databases int
}

// DefaultExternalDatabases is what a Redis serves unless it was configured
// otherwise, and what this provider assumes when the Connection does not
// say.
const DefaultExternalDatabases = 16

// externalConfig is the `redis` Connection's spec.config.
type externalConfig struct {
	// Usage is what the operator configured the server's maxmemory-policy
	// for: "cache" for an evicting server, "queue" for one that refuses
	// writes when full.
	Usage string `json:"usage,omitempty"`
	// Databases is how many logical databases the server offers.
	Databases int `json:"databases,omitempty"`
}

// NewExternal builds the provisioner from a Connection.
func NewExternal(opts Options) (*External, error) {
	if strings.TrimSpace(opts.URL) == "" {
		return nil, fmt.Errorf("a %s connection needs the server's URL in its credential (key %q)",
			ProviderRedis, CredentialKeyURL)
	}
	if _, err := url.Parse(opts.URL); err != nil {
		return nil, fmt.Errorf("the %s connection's url is not a URL: %w", ProviderRedis, err)
	}
	cfg := externalConfig{}
	if conn := opts.Connection; conn != nil && conn.Spec.Config != nil && len(conn.Spec.Config.Raw) > 0 {
		if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
			return nil, fmt.Errorf("invalid %s config: %w", ProviderRedis, err)
		}
	}
	databases := cfg.Databases
	if databases <= 0 {
		databases = DefaultExternalDatabases
	}
	return &External{URL: opts.URL, Usage: Usage(cfg.Usage), Databases: databases}, nil
}

// Provision hands over the server's own keyspace.
func (e *External) Provision(_ context.Context, name string) (Instance, error) {
	return e.instance(name, 0, Requirements{})
}

// ProvisionWith is Provision with the claim's requirements checked against
// what the operator said the server is.
func (e *External) ProvisionWith(_ context.Context, name string, req Requirements) (Instance, error) {
	if err := e.satisfies(req); err != nil {
		return Instance{}, err
	}
	return e.instance(name, 0, req)
}

// CreateBranch gives a preview its own logical database at the same server.
//
// It is a weaker boundary than the in-cluster provider's separate process
// and this package does not pretend otherwise: the preview cannot see
// production's keys, and a FLUSHALL from either empties both. The
// declaration says `fresh` because what the preview gets is empty; what it
// does not say is isolated, and the docs are explicit about the difference.
func (e *External) CreateBranch(_ context.Context, instanceID, name string) (Branch, error) {
	database := e.databaseFor(name)
	if database >= e.Databases {
		return Branch{}, fmt.Errorf("%w: this server offers %d logical databases and every one is spoken for; "+
			"a preview of this claim has nowhere to go. Point the claim at a %s connection, which gives every "+
			"preview an instance of its own", ErrUnsatisfiable, e.Databases, ProviderValkey)
	}
	instance, err := e.instance(instanceID+"/"+name, database, Requirements{})
	if err != nil {
		return Branch{}, err
	}
	return Branch{
		ID:          instance.ID,
		Binding:     instance.Binding,
		Provenance:  ProvenanceSynthetic,
		Tenancy:     TenancyShared,
		TenancyNote: instance.TenancyNote,
	}, nil
}

// Release does nothing: the credential is the server's own, handed over by
// whoever wrote the Connection, and this provider never minted one to take
// back.
func (e *External) Release(context.Context, string) error { return nil }

// Deprovision does nothing at a server the platform does not run: the
// keyspace is the server's, and emptying somebody else's database on the way
// out is not this provider's to do. The claim's binding Secret goes, which
// is what the platform owns.
func (e *External) Deprovision(context.Context, string) error { return nil }

// DeleteBranch is the same: the preview's binding goes with the preview, and
// what is in the server's database stays until somebody says otherwise.
func (e *External) DeleteBranch(context.Context, string, string) error { return nil }

// satisfies is the refusal this provider exists for.
func (e *External) satisfies(req Requirements) error {
	if req.Tenancy == TenancyDedicated {
		return fmt.Errorf("%w: this connection reaches a server the platform does not run, so it cannot be "+
			"given a server of its own. Claim through a %s connection to be given one",
			ErrUnsatisfiable, ProviderValkey)
	}
	if req.Tenancy != "" && !req.Tenancy.Known() {
		return fmt.Errorf("%w: tenancy %q is not one of %s", ErrUnsatisfiable, req.Tenancy, tenancyList())
	}
	if req.Usage != "" {
		switch {
		case e.Usage == "":
			return fmt.Errorf("%w: this connection does not say what the server is configured for, so a claim "+
				"asking for %s cannot be honoured. Set `usage` on the connection to the server's own "+
				"maxmemory-policy (%s), or claim through a %s connection, which configures the instance it "+
				"creates", ErrUnsatisfiable, req.Usage, usageList(), ProviderValkey)
		case e.Usage != req.Usage:
			return fmt.Errorf("%w: this server is configured for %s and the claim asks for %s. A %s served by "+
				"an evicting server drops work under memory pressure and reports nothing, which is what this "+
				"refusal exists to prevent", ErrUnsatisfiable, e.Usage, req.Usage, req.Usage)
		}
	}
	if req.MaxMemory != "" {
		return fmt.Errorf("%w: maxMemory is the server's own configuration and this connection reaches a server "+
			"the platform does not run. Claim through a %s connection to be given an instance with a limit of "+
			"its own", ErrUnsatisfiable, ProviderValkey)
	}
	if req.Version != "" {
		return fmt.Errorf("%w: version is the server's own and this connection reaches a server the platform "+
			"does not run. Claim through a %s connection to choose one", ErrUnsatisfiable, ProviderValkey)
	}
	return nil
}

// instance is the server's address with a logical database selected.
func (e *External) instance(name string, database int, _ Requirements) (Instance, error) {
	parsed, err := url.Parse(e.URL)
	if err != nil {
		return Instance{}, err
	}
	parsed.Path = "/" + fmt.Sprint(database)
	password := ""
	if parsed.User != nil {
		password, _ = parsed.User.Password()
	}
	port := parsed.Port()
	if port == "" {
		port = "6379"
	}
	return Instance{
		ID: name,
		Binding: Binding{
			URL:      parsed.String(),
			Host:     parsed.Hostname(),
			Port:     port,
			Password: password,
			TLS:      parsed.Scheme == "rediss",
		},
		// The server is somebody's production server as far as the platform
		// knows, and it declares nothing it cannot vouch for.
		Provenance: ProvenanceProduction,
		Tenancy:    TenancyShared,
		TenancyNote: fmt.Sprintf("logical database %d at a server the platform does not run: a database "+
			"number keeps this claim from reading the server's other keyspaces and does not keep a FLUSHALL "+
			"on either side from emptying both", database),
	}, nil
}

// databaseFor picks a logical database for a preview, deterministically, so
// that a preview reconciled twice gets the same one. Database 0 is the
// claim's own, so previews start at 1.
func (e *External) databaseFor(name string) int {
	if e.Databases <= 1 {
		return e.Databases
	}
	sum := 0
	for _, b := range []byte(name) {
		sum = (sum*31 + int(b)) % (e.Databases - 1)
	}
	return sum + 1
}
