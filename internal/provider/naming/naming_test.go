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

package naming

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// store is a provider's objects by name, with the project each records.
type store map[string]string

func (s store) lookup(_ context.Context, name string) (Owner, error) {
	project, found := s[name]
	return Owner{Found: found, Project: project}, nil
}

const (
	limit = 50
	// shopsDatabase is what project "shop"'s claim "db" is named, and
	// kitchen-db is what it was named before names carried the project.
	shopsDatabase = "kitchen-shop-db"
	legacyName    = "kitchen-db"
)

func provider(s store) Provider {
	return Provider{Kind: "database", Limit: limit, Lookup: s.lookup}
}

// The whole of the decision, one row per rule in the package comment.
func TestResolvePicksTheNameAndRefusesTheAdoptions(t *testing.T) {
	shopDB := Resource{Project: "shop", Claim: "db"}

	for _, testCase := range []struct {
		name    string
		res     Resource
		objects store
		want    string
		refuses string
	}{{
		name:    "a claim that has never bound gets the project in its name",
		res:     shopDB,
		objects: store{},
		want:    shopsDatabase,
	}, {
		name:    "a bound claim keeps the name it is bound to",
		res:     Resource{Project: "shop", Claim: "db", Name: legacyName},
		objects: store{legacyName: ""},
		want:    legacyName,
	}, {
		name:    "a claim bound before names carried the project keeps the unqualified one",
		res:     Resource{Project: "shop", Claim: "db", Unqualified: true},
		objects: store{legacyName: ""},
		want:    legacyName,
	}, {
		name:    "its own object under the qualified name is adopted",
		res:     shopDB,
		objects: store{shopsDatabase: "shop"},
		want:    shopsDatabase,
	}, {
		// Project "shop-d" claim "b" and project "shop" claim "db" both
		// spell kitchen-shop-db. The label is what tells them apart.
		name:    "a qualified name two projects can spell is refused on the label",
		res:     shopDB,
		objects: store{shopsDatabase: "shop-d"},
		refuses: `belongs to project "shop-d"`,
	}, {
		name:    "an object from before the project was in the name is not adopted",
		res:     shopDB,
		objects: store{legacyName: ""},
		refuses: "provisioned before Kitchen put the project in the name",
	}, {
		name:    "an operator hands the old object over by naming it on the claim",
		res:     Resource{Project: "shop", Claim: "db", HandOver: legacyName},
		objects: store{legacyName: ""},
		want:    legacyName,
	}, {
		name:    "a hand-over naming something else hands nothing over",
		res:     Resource{Project: "shop", Claim: "db", HandOver: "kitchen-other"},
		objects: store{legacyName: ""},
		refuses: "provisioned before Kitchen put the project in the name",
	}, {
		name:    "an old object already recorded as this project's needs no hand-over again",
		res:     shopDB,
		objects: store{legacyName: "shop"},
		want:    legacyName,
	}, {
		name:    "an old object recorded as another project's is refused outright",
		res:     shopDB,
		objects: store{legacyName: "warehouse"},
		refuses: `belongs to project "warehouse"`,
	}} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := Resolve(context.Background(), testCase.res, provider(testCase.objects))
			if testCase.refuses != "" {
				if !errors.Is(err, ErrNotAdoptable) {
					t.Fatalf("want ErrNotAdoptable, got %v", err)
				}
				if !strings.Contains(err.Error(), testCase.refuses) {
					t.Fatalf("the refusal does not say %q: %v", testCase.refuses, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Fatalf("want %q, got %q", testCase.want, got)
			}
		})
	}
}

// The bug this package exists for, in one test: project A's retained
// database is not what project B's claim of the same name binds to.
func TestOneProjectsRetainedResourceIsNotAnothersToAdopt(t *testing.T) {
	retained := store{shopsDatabase: "shop"}

	mine, err := Resolve(context.Background(), Resource{Project: "shop", Claim: "db"}, provider(retained))
	if err != nil {
		t.Fatal(err)
	}
	if mine != shopsDatabase {
		t.Fatalf("the project that owns it must get it back: %q", mine)
	}

	theirs, err := Resolve(context.Background(), Resource{Project: "warehouse", Claim: "db"}, provider(retained))
	if err != nil {
		t.Fatal(err)
	}
	if theirs == mine {
		t.Fatal("another project's claim of the same name resolved to the same database")
	}
}

// A hand-over is one object at a time: the annotation names what is being
// given away, so setting it cannot sweep up anything else.
func TestTheHandOverIsRefusedWhenTheClaimAlreadyHasItsOwn(t *testing.T) {
	objects := store{legacyName: "", shopsDatabase: "shop"}
	got, err := Resolve(context.Background(),
		Resource{Project: "shop", Claim: "db", HandOver: legacyName}, provider(objects))
	if err != nil {
		t.Fatal(err)
	}
	if got != shopsDatabase {
		t.Fatalf("a claim with a database of its own keeps it: %q", got)
	}
}

// A provider with no lookup — one that creates nothing to adopt — still gets
// the qualified name.
func TestAProviderWithNothingToLookUpStillGetsTheQualifiedName(t *testing.T) {
	got, err := Resolve(context.Background(), Resource{Project: "shop", Claim: "db"}, Provider{Kind: "keyspace"})
	if err != nil {
		t.Fatal(err)
	}
	if got != shopsDatabase {
		t.Fatalf("got %q", got)
	}
}

// Truncation replaces what it cut with a digest of the whole, so two names
// sharing a long prefix never land on one object.
func TestTruncationKeepsLongNamesApart(t *testing.T) {
	long := strings.Repeat("a", 60)
	left := Resource{Project: "shop", Claim: long + "-orders"}.Qualified(limit)
	right := Resource{Project: "shop", Claim: long + "-billing"}.Qualified(limit)
	if left == right {
		t.Fatalf("two claims resolved to one name: %q", left)
	}
	for _, name := range []string{left, right} {
		if len(name) > limit {
			t.Errorf("%q is over the budget", name)
		}
		if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
			t.Errorf("%q is not a DNS label", name)
		}
	}
	// Two projects whose names differ only past the budget are two names too.
	one := Resource{Project: long + "-one", Claim: "db"}.Qualified(limit)
	two := Resource{Project: long + "-two", Claim: "db"}.Qualified(limit)
	if one == two {
		t.Error("two projects collided on one name")
	}
	// A name that fits is left exactly as it is, lowercased.
	if got := Truncate("Kitchen-Shop-DB", limit); got != shopsDatabase {
		t.Errorf("a name that fits was rewritten to %q", got)
	}
	// No limit is no truncation, for a provider that imposes none.
	if got := Truncate(strings.Repeat("b", 300), 0); len(got) != 300 {
		t.Errorf("a provider with no limit had its name cut to %d", len(got))
	}
}
