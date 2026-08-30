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

package database

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The image catalogue: what the platform knows it can run a database from,
// and what each image promises the database will be able to CREATE EXTENSION.
//
// It is a compiled-in list rather than something read off a registry for the
// same reason BuildpacksBuilderImage is pinned: an installation should get the
// same answer to "can you give me PostGIS" today and next month, and the
// answer has to be available *before* anything is created — that is the whole
// point of refusing a claim rather than letting an application find out in a
// crash loop. An operator whose images are not these says so on the
// Connection, which is where an installation's own vocabulary belongs.

// PostgresImage is one image a database can be run from, with the majors it
// is published for and what it ships.
type PostgresImage struct {
	// Repository is the image without a tag; the major is the tag.
	Repository string `json:"repository"`
	// Majors are the Postgres major versions published under it, newest
	// first — the order is the preference, so an unversioned claim gets the
	// first entry rather than whatever sorts highest.
	Majors []string `json:"majors,omitempty"`
	// Extensions the image ships beyond what a bare Postgres has. The list is
	// a promise: the platform refuses a claim asking for anything not on it.
	Extensions []string `json:"extensions,omitempty"`
}

// Tag is the image reference for one major.
func (i PostgresImage) Tag(major string) string { return i.Repository + ":" + major }

// Supplies reports whether the image ships an extension, by its canonical
// name.
func (i PostgresImage) Supplies(extension string) bool {
	return slices.Contains(i.Extensions, extension) || slices.Contains(coreExtensions, extension)
}

// DefaultPostgresMajor is what a claim that names no version gets. It moves
// with the catalogue below and never on its own: a claim already provisioned
// keeps the major it was created with, because the image is recorded on the
// Cluster and a running database is never re-imaged under it.
const DefaultPostgresMajor = "17"

// supportedMajors is what CloudNativePG publishes images for, newest first.
var supportedMajors = []string{"18", "17", "16", "15", "14", "13"}

// coreExtensions are contrib modules every one of these images has, because
// they come with PostgreSQL itself. Keeping them out of the per-image lists
// is what stops the catalogue from being three copies of the same twenty
// names, and it is why Supplies consults both.
var coreExtensions = []string{
	"btree_gin", "btree_gist", "citext", "cube", "dblink", "earthdistance",
	"fuzzystrmatch", "hstore", "intarray", "isn", "ltree", "pg_prewarm",
	"pg_stat_statements", "pg_trgm", "pgcrypto", "postgres_fdw", "tablefunc",
	"tsm_system_rows", "unaccent", "uuid-ossp",
}

// DefaultPostgresImages is the catalogue as it ships, in preference order:
// the standard CloudNativePG image first, so a claim that asks for nothing in
// particular gets the ordinary one, and the PostGIS build only when something
// on it was asked for.
//
// The contents of each are the upstream flavours' own published lists, and
// they are pinned in the same sense DefaultCNPGChartVersion is: bumping the
// operator means reading the release notes for an extension that arrived or
// left, because an entry here is a promise the platform refuses claims on.
//
//   - ghcr.io/cloudnative-pg/postgresql is the *standard* flavour, which adds
//     pgaudit, pgvector and Postgres Failover Slots to the contrib set.
//   - ghcr.io/cloudnative-pg/postgis is that image with PostGIS and pgRouting
//     built on top, published for the same majors.
var DefaultPostgresImages = []PostgresImage{
	{
		Repository: "ghcr.io/cloudnative-pg/postgresql",
		Majors:     supportedMajors,
		Extensions: []string{"pgaudit", "pg_failover_slots", "vector"},
	},
	{
		Repository: "ghcr.io/cloudnative-pg/postgis",
		Majors:     supportedMajors,
		Extensions: []string{
			"pgaudit", "pg_failover_slots", "vector",
			"postgis", "postgis_raster", "postgis_topology",
			"postgis_tiger_geocoder", "address_standardizer", "pgrouting",
		},
	},
}

// extensionAliases are the names people write for an extension that is
// installed under another one. There are exactly two of them and both are
// mistakes worth being kind about: pgvector's extension is called vector, and
// uuid-ossp is spelled with a dash that an environment variable habit turns
// into an underscore.
var extensionAliases = map[string]string{
	"pgvector":  "vector",
	"uuid_ossp": "uuid-ossp",
}

// extensionName is what may be written into the bootstrap SQL. Extension
// names reach a CREATE EXTENSION statement built by the platform, so the
// alphabet is closed rather than escaped: letters, digits, underscore and the
// one dash uuid-ossp needs.
var extensionName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// canonicalExtension normalizes an extension as written on a claim.
func canonicalExtension(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if canonical, ok := extensionAliases[name]; ok {
		return canonical
	}
	return name
}

// Resolution is the answer to a claim's requirements: the image that serves
// them, and the extensions to create in the database once it is up.
type Resolution struct {
	// Image is the full reference, repository and major.
	Image string
	// Major is the Postgres major version chosen.
	Major string
	// Extensions are the canonical names, sorted, that the database is
	// bootstrapped with. Empty asks for nothing beyond a plain database.
	Extensions []string
}

// resolveImage picks the image for a claim's requirements, or refuses.
//
// The refusal is the point of the function. Everything it can say, it says in
// one sentence: which extension nothing supplies, which majors exist when the
// asked-for one does not, and which images were consulted. A claim that gets
// this message can be fixed by reading it; a claim that got a database and a
// crash loop three minutes later cannot.
func resolveImage(images []PostgresImage, req Requirements) (Resolution, error) {
	if len(images) == 0 {
		return Resolution{}, fmt.Errorf("%w: this connection has no Postgres images configured, so nothing "+
			"can be provisioned through it", ErrUnsatisfiable)
	}

	major := strings.TrimSpace(req.Version)
	if major == "" {
		major = defaultMajor(images)
	}
	if err := validMajor(major); err != nil {
		return Resolution{}, err
	}

	wanted := make([]string, 0, len(req.Extensions))
	for _, extension := range req.Extensions {
		canonical := canonicalExtension(extension)
		if canonical == "" {
			continue
		}
		if !extensionName.MatchString(canonical) {
			return Resolution{}, fmt.Errorf("%w: %q is not an extension name — they are the identifiers "+
				"CREATE EXTENSION takes, letters, digits and underscores", ErrUnsatisfiable, extension)
		}
		if !slices.Contains(wanted, canonical) {
			wanted = append(wanted, canonical)
		}
	}
	sort.Strings(wanted)

	majorServed := false
	for _, image := range images {
		if !slices.Contains(image.Majors, major) {
			continue
		}
		majorServed = true
		if missing := missingFrom(image, wanted); len(missing) == 0 {
			return Resolution{Image: image.Tag(major), Major: major, Extensions: wanted}, nil
		}
	}

	if !majorServed {
		return Resolution{}, fmt.Errorf("%w: no image here is published for Postgres %s; this connection has %s",
			ErrUnsatisfiable, major, strings.Join(availableMajors(images), ", "))
	}

	// One image at a time is the wrong thing to report — the claim asked for
	// a set, and what it needs to hear is which member of the set nothing can
	// supply, not which image happened to be checked last.
	unsupplied := unsupplied(images, major, wanted)
	return Resolution{}, fmt.Errorf(
		"%w: no image here supplies %s on Postgres %s. What is available: %s",
		ErrUnsatisfiable, strings.Join(unsupplied, " and "), major, catalogue(images, major))
}

// missingFrom is the requested extensions an image does not ship.
func missingFrom(image PostgresImage, wanted []string) []string {
	missing := make([]string, 0, len(wanted))
	for _, extension := range wanted {
		if !image.Supplies(extension) {
			missing = append(missing, extension)
		}
	}
	return missing
}

// unsupplied is what no image at this major ships. When every extension is
// supplied by *some* image but no single one supplies them all, that is worth
// saying as itself rather than reported as a missing extension nothing has.
func unsupplied(images []PostgresImage, major string, wanted []string) []string {
	nowhere := make([]string, 0, len(wanted))
	for _, extension := range wanted {
		found := false
		for _, image := range images {
			if slices.Contains(image.Majors, major) && image.Supplies(extension) {
				found = true
				break
			}
		}
		if !found {
			nowhere = append(nowhere, extension)
		}
	}
	if len(nowhere) > 0 {
		return nowhere
	}
	// Every one exists somewhere, no image has them together.
	return []string{strings.Join(wanted, " and ") + " together"}
}

// catalogue is the images and what they add, for a refusal message.
func catalogue(images []PostgresImage, major string) string {
	entries := make([]string, 0, len(images))
	for _, image := range images {
		if !slices.Contains(image.Majors, major) {
			continue
		}
		extensions := "the contrib extensions only"
		if len(image.Extensions) > 0 {
			extensions = strings.Join(image.Extensions, ", ")
		}
		entries = append(entries, fmt.Sprintf("%s (%s)", image.Repository, extensions))
	}
	return strings.Join(entries, "; ")
}

// defaultMajor is the newest major the catalogue's first image publishes that
// the platform's own default does not exceed — so an operator who pinned an
// older set of images gets a claim provisioned rather than refused for a
// version they never offered.
func defaultMajor(images []PostgresImage) string {
	for _, image := range images {
		if slices.Contains(image.Majors, DefaultPostgresMajor) {
			return DefaultPostgresMajor
		}
	}
	for _, image := range images {
		if len(image.Majors) > 0 {
			return image.Majors[0]
		}
	}
	return DefaultPostgresMajor
}

// availableMajors is every major any image publishes, newest first.
func availableMajors(images []PostgresImage) []string {
	majors := make([]string, 0, len(images)*4)
	for _, image := range images {
		for _, major := range image.Majors {
			if !slices.Contains(majors, major) {
				majors = append(majors, major)
			}
		}
	}
	sort.Slice(majors, func(i, j int) bool {
		left, lerr := strconv.Atoi(majors[i])
		right, rerr := strconv.Atoi(majors[j])
		if lerr != nil || rerr != nil {
			return majors[i] > majors[j]
		}
		return left > right
	})
	return majors
}

// validMajor refuses anything that is not a Postgres major version, before it
// can become an image tag.
func validMajor(major string) error {
	number, err := strconv.Atoi(major)
	if err != nil || number < 9 || number > 99 {
		return fmt.Errorf("%w: %q is not a Postgres major version — it is a number, like 17",
			ErrUnsatisfiable, major)
	}
	return nil
}
