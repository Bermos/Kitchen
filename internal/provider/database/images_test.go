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
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestAClaimThatAsksForNothingGetsThePlainImage(t *testing.T) {
	resolution, err := resolveImage(DefaultPostgresImages, Requirements{})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Major != DefaultPostgresMajor {
		t.Fatalf("major %q, want the platform default %q", resolution.Major, DefaultPostgresMajor)
	}
	if resolution.Image != "ghcr.io/cloudnative-pg/postgresql:"+DefaultPostgresMajor {
		t.Fatalf("unexpected image %q", resolution.Image)
	}
	if len(resolution.Extensions) != 0 {
		t.Fatalf("nothing was asked for, but %v was resolved", resolution.Extensions)
	}
}

func TestAnExtensionOnlyOneImageShipsChoosesThatImage(t *testing.T) {
	resolution, err := resolveImage(DefaultPostgresImages, Requirements{
		Version:    "16",
		Extensions: []string{"postgis", "pg_trgm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Image != "ghcr.io/cloudnative-pg/postgis:16" {
		t.Fatalf("image %q, want the postgis build", resolution.Image)
	}
	// Sorted and canonical, because they are written into bootstrap SQL and a
	// reconcile that runs twice must build the same statements.
	if !slices.Equal(resolution.Extensions, []string{"pg_trgm", "postgis"}) {
		t.Fatalf("unexpected extensions %v", resolution.Extensions)
	}
}

func TestPgvectorIsResolvedToTheExtensionItIsActuallyCalled(t *testing.T) {
	resolution, err := resolveImage(DefaultPostgresImages, Requirements{Extensions: []string{"pgvector"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resolution.Extensions, []string{"vector"}) {
		t.Fatalf("extensions %v, want the canonical name", resolution.Extensions)
	}
}

// The refusal is the feature: it has to name the extension nothing supplies,
// and say what there is instead, or the claim is no more use than a crash
// loop.
func TestAnExtensionNothingSuppliesIsRefusedWithWhatIsAvailable(t *testing.T) {
	_, err := resolveImage(DefaultPostgresImages, Requirements{Extensions: []string{"timescaledb"}})
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("error %v, want ErrUnsatisfiable", err)
	}
	message := err.Error()
	if !strings.Contains(message, "timescaledb") {
		t.Fatalf("the refusal does not name the extension: %q", message)
	}
	if !strings.Contains(message, "ghcr.io/cloudnative-pg/postgis") {
		t.Fatalf("the refusal does not say what is available: %q", message)
	}
}

func TestTwoExtensionsNoOneImageHasTogetherAreRefusedAsSuch(t *testing.T) {
	images := []PostgresImage{
		{Repository: "example.com/left", Majors: []string{"17"}, Extensions: []string{"alpha"}},
		{Repository: "example.com/right", Majors: []string{"17"}, Extensions: []string{"beta"}},
	}
	_, err := resolveImage(images, Requirements{Extensions: []string{"alpha", "beta"}})
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("error %v, want ErrUnsatisfiable", err)
	}
	if !strings.Contains(err.Error(), "together") {
		t.Fatalf("the refusal blames one extension rather than the pair: %q", err.Error())
	}
}

func TestAMajorNothingPublishesIsRefusedWithTheOnesThatAre(t *testing.T) {
	_, err := resolveImage(DefaultPostgresImages, Requirements{Version: "11"})
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("error %v, want ErrUnsatisfiable", err)
	}
	if !strings.Contains(err.Error(), "18") {
		t.Fatalf("the refusal does not list the majors that exist: %q", err.Error())
	}
}

func TestAVersionThatIsNotAVersionIsRefusedBeforeItBecomesATag(t *testing.T) {
	for _, version := range []string{"seventeen", "17.2", "latest", "17;rm -rf /"} {
		if _, err := resolveImage(DefaultPostgresImages, Requirements{Version: version}); !errors.Is(err, ErrUnsatisfiable) {
			t.Fatalf("version %q was accepted", version)
		}
	}
}

// Extension names reach a CREATE EXTENSION statement the platform builds, so
// the alphabet is closed rather than escaped.
func TestAnExtensionNameThatIsNotAnIdentifierIsRefused(t *testing.T) {
	for _, extension := range []string{`vector"; DROP DATABASE app; --`, "pg trgm", "1vector", "'x'"} {
		if _, err := resolveImage(DefaultPostgresImages, Requirements{Extensions: []string{extension}}); !errors.Is(err, ErrUnsatisfiable) {
			t.Fatalf("extension %q was accepted", extension)
		}
	}
}

func TestAContribExtensionNeedsNoEntryInTheCatalogue(t *testing.T) {
	resolution, err := resolveImage(DefaultPostgresImages, Requirements{Extensions: []string{"pgcrypto"}})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Image != "ghcr.io/cloudnative-pg/postgresql:"+DefaultPostgresMajor {
		t.Fatalf("image %q, want the plain one — contrib needs no special build", resolution.Image)
	}
}

func TestAConnectionWithItsOwnImagesUsesThemInstead(t *testing.T) {
	images := []PostgresImage{
		{Repository: "registry.internal/postgres", Majors: []string{"16"}, Extensions: []string{"timescaledb"}},
	}
	resolution, err := resolveImage(images, Requirements{Extensions: []string{"timescaledb"}})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Image != "registry.internal/postgres:16" {
		t.Fatalf("image %q, want the connection's own", resolution.Image)
	}
	// The platform's default major is not published here, so the catalogue's
	// own newest is what an unversioned claim gets rather than a refusal for
	// a version this installation never offered.
	if resolution.Major != "16" {
		t.Fatalf("major %q, want the catalogue's own", resolution.Major)
	}
}
