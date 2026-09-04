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
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// External is a server somebody else runs, reached over the URL its
// Connection holds: Upstash, ElastiCache, Aiven, or the Valkey a team
// already has.
//
// It provisions nothing. What it does is give each claim a keyspace of its
// own at that server — a logical database, allocated to that claim and
// written down on the Connection — and refuse what the server cannot
// honour.
//
// **The allocation and the refusal are the whole of this provider's
// design.** A logical database is not much of a boundary and this package
// says so at length, which is why the in-cluster provider does not use one;
// here there is no choice, because nobody can ask somebody else's Redis for
// another process. So what can be got right is got right: two claims never
// land on one database, an allocation survives every reconcile because it is
// a record rather than a hash of the name, and a server with nothing left is
// told so rather than quietly sharing database 0. What cannot be got right
// is said plainly instead — every claim through one Connection is handed the
// same password, so the separation is logical and not cryptographic, and
// docs/api/claims.md says which claims should not rely on it.
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
	// Ledger is where the allocations are kept, so that a database handed to
	// one claim is not handed to another.
	Ledger DatabaseLedger
	// ConnectionName is what the Connection is called, for the refusals a
	// person reads.
	ConnectionName string
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
	conn := opts.Connection
	if conn == nil {
		return nil, fmt.Errorf("a %s provisioner is built from a connection and was given none", ProviderRedis)
	}
	// The allocations live on the Connection's status, so this provider
	// needs the platform's own cluster even though it provisions nothing at
	// the server. Without it there is nowhere to record which claim holds
	// which database, and a provider that cannot record one would fall back
	// to sharing database 0 — which is the bug this exists to close.
	if opts.Cluster == nil {
		return nil, fmt.Errorf("a %s provisioner records each claim's logical database on its connection and was "+
			"given no client to do it with", ProviderRedis)
	}
	cfg := externalConfig{}
	if conn.Spec.Config != nil && len(conn.Spec.Config.Raw) > 0 {
		if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
			return nil, fmt.Errorf("invalid %s config: %w", ProviderRedis, err)
		}
	}
	databases := cfg.Databases
	if databases <= 0 {
		databases = DefaultExternalDatabases
	}
	return &External{
		URL:            opts.URL,
		Usage:          Usage(cfg.Usage),
		Databases:      databases,
		ConnectionName: conn.Name,
		Ledger: &connectionLedger{
			client: opts.Cluster,
			key:    types.NamespacedName{Namespace: conn.Namespace, Name: conn.Name},
		},
	}, nil
}

// Provision hands over a keyspace at the server.
func (e *External) Provision(ctx context.Context, res naming.Resource) (Instance, error) {
	return e.ProvisionWith(ctx, res, Requirements{})
}

// ProvisionWith is Provision with the claim's requirements checked against
// what the operator said the server is.
//
// There is nothing at the server to look a name up against — this provider
// creates nothing, so nothing of another project's can be adopted here — and
// the name is what the claim's database is recorded under.
func (e *External) ProvisionWith(ctx context.Context, res naming.Resource, req Requirements) (Instance, error) {
	if err := e.satisfies(req); err != nil {
		return Instance{}, err
	}
	name, err := naming.Resolve(ctx, res, naming.Provider{Kind: "keyspace"})
	if err != nil {
		return Instance{}, err
	}
	// A claim that is already bound was bound to database 0, because that is
	// what every binding this provider made before it allocated anything
	// selected. It keeps it: moving a bound claim's keyspace would hand the
	// application an empty one and leave its data where nothing reads it.
	bound := res.Name != "" || res.Unqualified
	database, err := e.database(ctx, name, bound)
	if err != nil {
		return Instance{}, err
	}
	return e.instance(name, database)
}

// CreateBranch gives a preview a logical database of its own at the same
// server, allocated from the same record as every claim's — so no two live
// previews, and no preview and claim, are ever handed one database between
// them.
//
// It is a weaker boundary than the in-cluster provider's separate process
// and this package does not pretend otherwise: the preview cannot see
// production's keys, and a FLUSHALL from either empties both. The
// declaration says `fresh` because what the preview gets is a database of
// its own; what it does not say is isolated, and the docs are explicit about
// the difference.
func (e *External) CreateBranch(ctx context.Context, instanceID, name string) (Branch, error) {
	holder := branchHolder(instanceID, name)
	database, err := e.database(ctx, holder, false)
	if err != nil {
		return Branch{}, err
	}
	instance, err := e.instance(holder, database)
	if err != nil {
		return Branch{}, err
	}
	return Branch{ID: instance.ID, Binding: instance.Binding, Provenance: ProvenanceSynthetic}, nil
}

// Deprovision destroys nothing at a server the platform does not run: the
// keyspace is the server's, and emptying somebody else's database on the way
// out is not this provider's to do. The claim's binding Secret goes, which
// is what the platform owns, and the database goes back into the pool —
// this is deletionPolicy Delete, which is the policy that says the data is
// finished with.
func (e *External) Deprovision(ctx context.Context, instanceID string) error {
	return e.release(ctx, instanceID)
}

// DeleteBranch gives the preview's database back, so that fifty previews
// over a month do not exhaust a server with sixteen databases. What is in it
// stays: the platform cannot empty a server it does not run, which is why a
// database that has never been handed out is preferred to one that has.
func (e *External) DeleteBranch(ctx context.Context, _, branchID string) error {
	return e.release(ctx, branchID)
}

// branchHolder is what a preview's database is recorded under, and the ID
// its branch is addressed by afterwards.
func branchHolder(instanceID, environment string) string {
	return instanceID + "/" + environment
}

// satisfies is the refusal this provider exists for.
func (e *External) satisfies(req Requirements) error {
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

// database is the logical database this holder has, allocating one when it
// has none. It is the same number on every reconcile because it is read back
// out of the record rather than worked out again.
func (e *External) database(ctx context.Context, holder string, bound bool) (int, error) {
	if e.Ledger == nil {
		return 0, fmt.Errorf("this %s provisioner has nowhere to record which database %q holds", ProviderRedis, holder)
	}
	database := 0
	err := e.Ledger.Update(ctx, func(holdings []DatabaseHolding) ([]DatabaseHolding, bool, error) {
		if held, ok := heldBy(holdings, holder); ok {
			database = held
			return holdings, false, nil
		}
		allocated, updated, err := e.allocate(holdings, holder, bound)
		if err != nil {
			return nil, false, err
		}
		database = allocated
		return updated, true, nil
	})
	if err != nil {
		return 0, err
	}
	return database, nil
}

// release gives a holder's database back, keeping the row: the database has
// been used, the platform cannot empty it, and allocate prefers one that
// never has been.
func (e *External) release(ctx context.Context, holder string) error {
	if e.Ledger == nil {
		return nil
	}
	return e.Ledger.Update(ctx, func(holdings []DatabaseHolding) ([]DatabaseHolding, bool, error) {
		changed := false
		for i := range holdings {
			if holdings[i].Holder == holder {
				holdings[i].Holder = ""
				changed = true
			}
		}
		return holdings, changed, nil
	})
}

// allocate picks this holder's database out of what is left, and answers the
// record with the allocation in it.
//
// Database 0 is never picked. Every binding this provider made before it
// allocated anything selected it, and a claim that has not been reconciled
// since is still using it without anything here saying so — so it belongs to
// them, and a claim that was already bound is put back on it rather than
// moved.
//
// Of what is left, a database nothing has ever held is preferred to one that
// has been given back. The platform cannot empty a database at a server it
// does not run, so a reused one still holds whatever the last holder left in
// it; taking the untouched ones first is what keeps a preview from
// inheriting a previous preview's keys for as long as the server has a
// database to spare.
func (e *External) allocate(holdings []DatabaseHolding, holder string, bound bool) (int, []DatabaseHolding, error) {
	if bound {
		return LegacyDatabase, append(holdings, DatabaseHolding{Database: LegacyDatabase, Holder: holder}), nil
	}

	seen := map[int]bool{}
	held := map[int]bool{}
	for _, holding := range holdings {
		seen[holding.Database] = true
		if holding.Holder != "" {
			held[holding.Database] = true
		}
	}
	for database := FirstAllocatableDatabase; database < e.Databases; database++ {
		if !seen[database] {
			return database, append(holdings, DatabaseHolding{Database: database, Holder: holder}), nil
		}
	}
	for database := FirstAllocatableDatabase; database < e.Databases; database++ {
		if !held[database] {
			for i := range holdings {
				if holdings[i].Database == database && holdings[i].Holder == "" {
					holdings[i].Holder = holder
					return database, holdings, nil
				}
			}
		}
	}
	return 0, nil, e.exhausted()
}

// exhausted is the refusal that replaced sharing database 0 with whoever was
// there first. It names the constraint, because the number of databases a
// server serves is the operator's to change and nobody guesses it.
func (e *External) exhausted() error {
	if e.Databases <= FirstAllocatableDatabase {
		return fmt.Errorf("%w: connection %q says its server offers %d logical database(s), and database %d is "+
			"never allocated — every binding made before Kitchen gave each claim one of its own selected it. "+
			"Set `databases` on the connection to what the server actually serves (a Redis serves %d unless it "+
			"was configured otherwise), or claim through a %s connection, which gives every claim an instance "+
			"of its own", ErrUnsatisfiable, e.ConnectionName, e.Databases, LegacyDatabase,
			DefaultExternalDatabases, ProviderValkey)
	}
	return fmt.Errorf("%w: connection %q reaches a server with %d logical databases and databases %d-%d are all "+
		"held; database %d is never allocated, because every binding made before Kitchen gave each claim one of "+
		"its own selected it. Raise `databases` on the connection if the server serves more (a Redis serves %d "+
		"unless it was configured otherwise), delete a claim or a preview that no longer needs one, or claim "+
		"through a %s connection, which gives every claim an instance of its own",
		ErrUnsatisfiable, e.ConnectionName, e.Databases, FirstAllocatableDatabase, e.Databases-1, LegacyDatabase,
		DefaultExternalDatabases, ProviderValkey)
}

// heldBy is the database this holder already has.
func heldBy(holdings []DatabaseHolding, holder string) (int, bool) {
	for _, holding := range holdings {
		if holding.Holder == holder {
			return holding.Database, true
		}
	}
	return 0, false
}

// instance is the server's address with a logical database selected.
func (e *External) instance(name string, database int) (Instance, error) {
	parsed, err := url.Parse(e.URL)
	if err != nil {
		return Instance{}, err
	}
	parsed.Path = "/" + strconv.Itoa(database)
	password := ""
	if parsed.User != nil {
		password, _ = parsed.User.Password()
	}
	port := parsed.Port()
	if port == "" {
		port = "6379"
	}
	return Instance{
		ID:   name,
		Name: name,
		Binding: Binding{
			URL:      parsed.String(),
			Host:     parsed.Hostname(),
			Port:     port,
			Password: password,
			Database: strconv.Itoa(database),
			TLS:      parsed.Scheme == "rediss",
		},
		// The server is somebody's production server as far as the platform
		// knows, and it declares nothing it cannot vouch for.
		Provenance: ProvenanceProduction,
	}, nil
}
