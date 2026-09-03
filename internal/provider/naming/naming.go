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

// Package naming is the one place a provider-side object gets its name.
//
// Every provisioner creates its objects under a deterministic name, which is
// what makes provisioning restartable: a lost status is recovered by looking
// the name up rather than by provisioning twice. That name used to be
// kitchen-<claim>, and a name built out of the claim alone is a name any
// project can produce. Under the default deletionPolicy Retain a deleted
// claim leaves its database, bucket or cache instance behind, so a developer
// of another project creating a claim of the same name against the same
// Connection was handed the first project's data — bound to it, with a
// freshly issued credential, and nothing anywhere saying so.
//
// So a name carries the project: kitchen-<project>-<claim>, cut to whatever
// the provider's own budget is with a digest of the whole replacing what was
// cut (Truncate), and the object is labelled or tagged with the project as
// well, because two projects can still spell one name between them —
// project "a-b" claim "c" and project "a" claim "b-c" both read
// kitchen-a-b-c.
//
// Resolve is the decision every provisioner makes before it creates
// anything, and it is here rather than in each of them so that all four
// answer it the same way:
//
//   - A claim already bound keeps the name it is bound to, forever. Renaming
//     a bound claim's resource would leave its data behind under the old
//     name and hand the application an empty one.
//   - An object under the qualified name is adopted when it is this
//     project's and refused when it is another's.
//   - An object under the old, unqualified name is *not* adopted. Nothing
//     records which project's data is in it, and guessing is the whole bug.
//     The claim fails with a message naming the object and the annotation an
//     operator sets to hand it over, so nothing is destroyed and nothing is
//     silently taken.
package naming

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	// Prefix is what every provider-side object Kitchen creates is named
	// with, so that an object of somebody else's is recognisable as such.
	Prefix = "kitchen-"

	// LabelProject records which project a provider-side object belongs to.
	// It is the same label the platform puts on everything else a project
	// owns; it is spelled here as well because a provisioner cannot import
	// the controller that spells it there.
	LabelProject = "kitchen.bermos.dev/project"

	// AdoptAnnotation is how an operator hands an object named before names
	// carried the project to the claim that should have it. Its value is the
	// object's own name — spelling it out is what makes the hand-over a
	// deliberate act on one object rather than a switch that adopts whatever
	// happens to be lying around.
	//
	// It is set on the ResourceClaim, and the API does not take annotations
	// from a request body: this is an operator's act on the cluster, not
	// something a project's developer can ask for.
	AdoptAnnotation = "kitchen.bermos.dev/adopt-instance"

	// digestLength is how much of the digest replaces what a truncation cut.
	digestLength = 8
)

// ErrNotAdoptable marks an existing provider-side object this claim will not
// bind to: another project's, or one from before names carried the project.
// Nothing was created and retrying alone will refuse again — a person has to
// act — so the reconciler lands it on the claim as a failure with the
// message attached rather than requeueing in silence.
var ErrNotAdoptable = errors.New("provider-side object cannot be adopted")

// Resource is the claim a provisioner is naming an object for.
type Resource struct {
	// Project and Claim are what the name is built from.
	Project string
	Claim   string

	// Name is the provider-side name this claim's resource is already known
	// by, off the claim's status. A bound claim keeps it whatever the naming
	// rules have since become.
	Name string

	// Unqualified marks a claim bound before names carried the project,
	// whose status therefore records no name: it keeps the unqualified one,
	// cut to whatever the provider's budget makes of it.
	Unqualified bool

	// HandOver is the name an operator has handed to this claim through the
	// adopt annotation, empty on almost every claim there will ever be.
	HandOver string
}

// Owner is what a provisioner found under a name it was asked about.
type Owner struct {
	// Found says an object of that name is there.
	Found bool
	// Project is the project recorded on it. Empty means the provider
	// records none — Neon has nowhere to put it — or that the object
	// predates project-qualified naming.
	Project string
}

// Lookup answers Owner for one name at one provider. Absent is Owner{} and
// no error; only a provider that could not be asked returns one.
type Lookup func(ctx context.Context, name string) (Owner, error)

// Provider is what a provisioner tells Resolve about itself: what it calls
// the things it makes, for the message a person reads; how long a name it
// takes, zero meaning no limit; and how to look one up.
type Provider struct {
	Kind   string
	Limit  int
	Lookup Lookup
}

// Qualified is the name a new resource of this claim gets.
func (r Resource) Qualified(limit int) string {
	return Truncate(Prefix+r.Project+"-"+r.Claim, limit)
}

// Legacy is the name this claim's resource would have had before names
// carried the project.
func (r Resource) Legacy(limit int) string {
	return Truncate(Prefix+r.Claim, limit)
}

// Resolve picks the provider-side name for a claim's resource and refuses
// the adoptions that would hand one project another's data. See the package
// comment for the rules; they are in that order here.
func Resolve(ctx context.Context, res Resource, p Provider) (string, error) {
	if res.Name != "" {
		return res.Name, nil
	}
	if res.Unqualified {
		return res.Legacy(p.Limit), nil
	}

	qualified := res.Qualified(p.Limit)
	if p.Lookup == nil {
		return qualified, nil
	}

	owner, err := p.Lookup(ctx, qualified)
	if err != nil {
		return "", err
	}
	if owner.Found {
		if owner.Project != "" && owner.Project != res.Project {
			return "", fmt.Errorf("%w: the %s named %q belongs to project %q, and this claim is project %q's — "+
				"rename the claim, or claim through a connection of your own",
				ErrNotAdoptable, p.Kind, qualified, owner.Project, res.Project)
		}
		return qualified, nil
	}

	legacy := res.Legacy(p.Limit)
	if legacy == qualified {
		return qualified, nil
	}
	previous, err := p.Lookup(ctx, legacy)
	if err != nil {
		return "", err
	}
	switch {
	case !previous.Found:
		return qualified, nil
	case previous.Project == res.Project:
		// Handed over on an earlier reconcile, and labelled with this
		// project when it was: it is this claim's, by record rather than by
		// coincidence of name.
		return legacy, nil
	case previous.Project != "":
		return "", fmt.Errorf("%w: the %s named %q belongs to project %q, and this claim is project %q's",
			ErrNotAdoptable, p.Kind, legacy, previous.Project, res.Project)
	case res.HandOver == legacy:
		return legacy, nil
	}
	return "", fmt.Errorf("%w: a %s named %q is already there. It was provisioned before Kitchen put the project "+
		"in the name, so nothing records whose data is in it and this claim will not take it. An operator who "+
		"knows it is project %q's hands it over by annotating this claim %s=%s; leaving it alone leaves the "+
		"data where it is and this claim unbound, and deleting the %s at the provider is the other way out",
		ErrNotAdoptable, p.Kind, legacy, res.Project, AdoptAnnotation, legacy, p.Kind)
}

// Truncate shortens a name to fit, and replaces what it cut with a digest of
// the whole rather than simply dropping it: two claims whose names share a
// long prefix would otherwise land on one object, which is two projects
// sharing one database and the worst outcome any of this can have. A limit
// of zero is no limit, for a provider that imposes none.
func Truncate(name string, limit int) string {
	name = strings.ToLower(name)
	if limit <= 0 || len(name) <= limit {
		return strings.Trim(name, "-")
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:digestLength]
	return strings.Trim(name[:limit-len(suffix)], "-") + suffix
}

// Join builds a name out of parts under one budget — a preview's resource
// beside its parent's, where the parent's half is trimmed first so that the
// half that makes it unique keeps the most room.
func Join(head string, headLimit int, tail string, limit int) string {
	return Truncate(Truncate(head, headLimit)+"-"+tail, limit)
}
