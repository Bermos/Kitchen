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

// Package artifact stores what a build produced beside its image — an
// offering's OpenAPI document today (#498), whatever else #187 comes to
// carry — and reads it back.
//
// # Where an artifact lives, and why there
//
// One blob per artifact, in a manifest whose `subject` is the image the build
// pushed. That is the OCI 1.1 referrers relationship, the same one
// internal/attestation uses for evidence, and it is chosen for a property
// nothing else has: a referrer is deleted with the image it refers to and is
// safe from a registry's garbage collection while that image lives. So an
// artifact inherits the release's lifetime and needs no retention policy, no
// PVC and no object store — and it is backed up exactly as much as the image
// it belongs to, which docs/BACKUP.md already accounts for.
//
// # Identified by content
//
// A blob is addressed by its own digest, so two builds of a commit range that
// never touched the document store one blob however many builds point at it,
// and two releases carry the same specification exactly when their recorded
// digests are equal. That is what keeps the deferred contract-versioning work
// in #489 possible for free: comparing two releases' contracts is fetching two
// digests, with nothing to migrate.
//
// # One mechanism, many types
//
// An artifact is a name, a type, a media type and bytes. Adding a type is
// adding a value to ReleaseArtifactType and a media type beside it — the
// attach, the listing and the fetch below do not change shape for it.
package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	// ArtifactType is what the referrers listing reports this manifest as,
	// which is how a reader tells Kitchen's release artifacts from the
	// evidence, the signatures and the SBOMs that may also refer to one
	// image. It reaches the listing through the manifest's config media
	// type, which is what OCI 1.1 says to fall back to when a manifest
	// declares no artifactType of its own.
	ArtifactType = "application/vnd.kitchen.release-artifacts.v1+json"

	// attachmentSuffix names the manifest under a tag as well as through the
	// referrers API, so a registry that implements neither still answers.
	// It is the same trade internal/attestation makes with cosign's `.att`.
	attachmentSuffix = ".artifacts"

	// nameAnnotation and typeAnnotation are how a layer says which artifact
	// it is without being pulled. They are outside anything signed and are
	// treated as a lookup key rather than as an assertion.
	nameAnnotation = "dev.bermos.kitchen.artifact.name"
	typeAnnotation = "dev.bermos.kitchen.artifact.type"
	pathAnnotation = "dev.bermos.kitchen.artifact.path"

	// MediaTypeOpenAPIJSON and MediaTypeOpenAPIYAML are the registered media
	// types for an OpenAPI document. Which one an artifact carries is
	// decided by what the file parsed as, never by its extension: a
	// specification served as the wrong type is one a generator refuses.
	MediaTypeOpenAPIJSON = "application/vnd.oai.openapi+json"
	MediaTypeOpenAPIYAML = "application/vnd.oai.openapi"
)

// ErrNotFound is what Fetch answers for an artifact that is not attached to
// this image. It is an answer rather than a failure: a release built before
// an offering declared a contract has none, and a reader says so.
var ErrNotFound = errors.New("no such artifact is attached to this image")

// Artifact is one thing a build extracted, on its way into the registry.
type Artifact struct {
	// Name identifies it within the build — an offering's name, for a
	// contract.
	Name string
	// Type is what it is.
	Type kitchenv1alpha1.ReleaseArtifactType
	// MediaType is what it is stored and served as.
	MediaType string
	// Path is where it came from in the repository, carried so that a
	// reader can say where to change it.
	Path string
	// Content is the bytes as they were read. They are stored unaltered:
	// an artifact the platform rewrote would no longer be the fact about
	// the source it exists to be.
	Content []byte
}

// Stored is one artifact as the registry holds it — the index row a Build
// records, without the content.
type Stored struct {
	Name      string
	Type      kitchenv1alpha1.ReleaseArtifactType
	MediaType string
	Path      string
	Digest    string
	Size      int64
}

// Status is the Stored row as a Build's status carries it, which is the one
// place the two spellings are joined.
func (s Stored) Status() kitchenv1alpha1.ReleaseArtifactStatus {
	return kitchenv1alpha1.ReleaseArtifactStatus{
		Name:      s.Name,
		Type:      s.Type,
		Path:      s.Path,
		MediaType: s.MediaType,
		Digest:    s.Digest,
		Size:      s.Size,
	}
}

// Store reads and writes release artifacts in an OCI registry.
//
// Like internal/attestation's it holds no state about what it has written:
// everything it knows it reads back out of the registry, so an artifact is
// retrievable from a digest alone with no lookup in Kitchen.
type Store struct {
	// Auth is the credential for the registry. Nil means anonymous, which
	// works for a public registry and for nothing Kitchen bundles.
	Auth authn.Authenticator

	// Transport overrides the HTTP transport. Tests use it to reach an
	// in-process registry; the operator leaves it nil.
	Transport http.RoundTripper

	// PlainHTTP talks to the registry over HTTP, for tests against an
	// in-process registry and nothing else.
	PlainHTTP bool
}

// Attach stores artifacts against an image's digest and answers the index
// rows to record, in the order they were given.
//
// It is idempotent by content: an artifact whose bytes are already attached
// under the same name is recognised and not attached twice, so a reconcile
// that runs again does not grow the manifest. An artifact whose *content*
// changed under a name that is already there is appended, and the newest
// layer with that name is the one Fetch answers — which is what makes a
// rebuild of the same commit, with the document fixed, do the obvious thing.
func (s *Store) Attach(ctx context.Context, imageRef string, artifacts []Artifact) ([]Stored, error) {
	if len(artifacts) == 0 {
		return nil, nil
	}
	subject, err := s.digestRef(imageRef)
	if err != nil {
		return nil, err
	}
	options := s.options(ctx)
	attachment := attachmentTag(subject)

	image, err := s.attachmentImage(ctx, attachment)
	if err != nil {
		return nil, err
	}

	stored := make([]Stored, 0, len(artifacts))
	changed := false
	for _, artifact := range artifacts {
		layer := static.NewLayer(artifact.Content, types.MediaType(artifact.MediaType))
		digest, err := layer.Digest()
		if err != nil {
			return nil, err
		}
		stored = append(stored, Stored{
			Name:      artifact.Name,
			Type:      artifact.Type,
			MediaType: artifact.MediaType,
			Path:      artifact.Path,
			Digest:    digest.String(),
			Size:      int64(len(artifact.Content)),
		})

		already, err := hasLayer(image, digest)
		if err != nil {
			return nil, err
		}
		if already {
			continue
		}
		image, err = mutate.Append(image, mutate.Addendum{
			Layer: layer,
			Annotations: map[string]string{
				nameAnnotation: artifact.Name,
				typeAnnotation: string(artifact.Type),
				pathAnnotation: artifact.Path,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("adding %s to the release's artifacts failed: %w", artifact.Name, err)
		}
		changed = true
	}
	if !changed {
		return stored, nil
	}
	image = mutate.ConfigMediaType(mutate.MediaType(image, types.OCIManifestSchema1), ArtifactType)

	// The subject descriptor has to describe the manifest as the registry
	// holds it, so it is read from the registry rather than assembled here:
	// a subject whose size or media type is guessed produces a referrers
	// entry pointing at nothing.
	descriptor, err := remote.Head(subject, options...)
	if err != nil {
		return nil, fmt.Errorf("the image %s could not be found in the registry: %w", subject, err)
	}
	linked, ok := mutate.Subject(image, *descriptor).(v1.Image)
	if !ok {
		return nil, errors.New("attaching the subject to the artifact manifest produced something that is not an image")
	}
	if err := remote.Write(attachment, linked, options...); err != nil {
		return nil, fmt.Errorf("writing the release's artifacts to %s failed: %w", attachment, err)
	}
	return stored, nil
}

// List is every artifact attached to an image, without its content.
func (s *Store) List(ctx context.Context, imageRef string) ([]Stored, error) {
	subject, err := s.digestRef(imageRef)
	if err != nil {
		return nil, err
	}
	manifests, err := s.manifests(ctx, subject)
	if err != nil {
		return nil, err
	}
	found := []Stored{}
	seen := map[string]bool{}
	for _, image := range manifests {
		manifest, err := image.Manifest()
		if err != nil {
			return nil, err
		}
		for _, descriptor := range manifest.Layers {
			digest := descriptor.Digest.String()
			if seen[digest] {
				continue
			}
			seen[digest] = true
			found = append(found, Stored{
				Name:      descriptor.Annotations[nameAnnotation],
				Type:      kitchenv1alpha1.ReleaseArtifactType(descriptor.Annotations[typeAnnotation]),
				MediaType: string(descriptor.MediaType),
				Path:      descriptor.Annotations[pathAnnotation],
				Digest:    digest,
				Size:      descriptor.Size,
			})
		}
	}
	return found, nil
}

// Fetch reads one artifact back by the digest the build recorded.
//
// It is addressed by digest rather than by name on purpose: the digest is
// what the Build's index row carries, so a link into an old release resolves
// to what that release shipped even after the name has been reused, and a
// reader that has the row never has to search the manifest for it.
func (s *Store) Fetch(ctx context.Context, imageRef, digest string) ([]byte, string, error) {
	subject, err := s.digestRef(imageRef)
	if err != nil {
		return nil, "", err
	}
	manifests, err := s.manifests(ctx, subject)
	if err != nil {
		return nil, "", err
	}
	for _, image := range manifests {
		manifest, err := image.Manifest()
		if err != nil {
			return nil, "", err
		}
		for _, descriptor := range manifest.Layers {
			if descriptor.Digest.String() != digest {
				continue
			}
			layer, err := image.LayerByDigest(descriptor.Digest)
			if err != nil {
				return nil, "", err
			}
			reader, err := layer.Compressed()
			if err != nil {
				return nil, "", err
			}
			body, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil {
				return nil, "", readErr
			}
			if closeErr != nil {
				return nil, "", closeErr
			}
			return body, string(descriptor.MediaType), nil
		}
	}
	return nil, "", fmt.Errorf("%w: %s", ErrNotFound, digest)
}

// manifests is every manifest that might hold this image's artifacts: what
// refers to it, plus the attachment tag for a registry that answers neither.
func (s *Store) manifests(ctx context.Context, subject name.Digest) ([]v1.Image, error) {
	options := s.options(ctx)
	images := []v1.Image{}

	index, err := remote.Referrers(subject, options...)
	if err != nil {
		return nil, fmt.Errorf("listing what refers to %s failed: %w", subject, err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, err
	}
	for _, descriptor := range manifest.Manifests {
		if descriptor.ArtifactType != ArtifactType {
			// Evidence, a signature, an SBOM attached by other tooling.
			// Not this reader's business.
			continue
		}
		image, err := remote.Image(subject.Context().Digest(descriptor.Digest.String()), options...)
		if err != nil {
			return nil, fmt.Errorf("fetching the artifacts %s failed: %w", descriptor.Digest, err)
		}
		images = append(images, image)
	}

	attachment, err := s.attachmentImage(ctx, attachmentTag(subject))
	if err != nil {
		return nil, err
	}
	// A registry with no attachment tag answers the empty image, whose
	// layers are none — merging it in costs nothing and saves a branch.
	return append(images, attachment), nil
}

// attachmentImage reads the manifest under the attachment tag, answering the
// empty image when there is none yet: a first attach is the ordinary case,
// not a fault.
func (s *Store) attachmentImage(ctx context.Context, tag name.Tag) (v1.Image, error) {
	image, err := remote.Image(tag, s.options(ctx)...)
	if err == nil {
		return image, nil
	}
	var transportErr *transport.Error
	if errors.As(err, &transportErr) && transportErr.StatusCode == http.StatusNotFound {
		return empty.Image, nil
	}
	return nil, fmt.Errorf("reading the artifacts attached at %s failed: %w", tag, err)
}

func hasLayer(image v1.Image, digest v1.Hash) (bool, error) {
	manifest, err := image.Manifest()
	if err != nil {
		return false, err
	}
	for _, descriptor := range manifest.Layers {
		if descriptor.Digest == digest {
			return true, nil
		}
	}
	return false, nil
}

// attachmentTag is where the manifest is also pushed: `sha256-<hex>.artifacts`
// in the image's own repository.
func attachmentTag(subject name.Digest) name.Tag {
	return subject.Context().Tag(strings.Replace(subject.DigestStr(), ":", "-", 1) + attachmentSuffix)
}

func (s *Store) digestRef(imageRef string) (name.Digest, error) {
	options := []name.Option{}
	if s.PlainHTTP {
		options = append(options, name.Insecure)
	}
	reference, err := name.NewDigest(imageRef, options...)
	if err != nil {
		return name.Digest{}, fmt.Errorf(
			"release artifacts are attached to a digest, and %q is not one — an image referenced by tag is "+
				"a moving target: %w", imageRef, err)
	}
	return reference, nil
}

func (s *Store) options(ctx context.Context) []remote.Option {
	options := []remote.Option{remote.WithContext(ctx)}
	if s.Auth != nil {
		options = append(options, remote.WithAuth(s.Auth))
	}
	if s.Transport != nil {
		options = append(options, remote.WithTransport(s.Transport))
	}
	return options
}

// OpenAPIMediaType decides how a document is stored from what it *is* rather
// than from what it is called: a file that parses as JSON is served as
// `+json`, and everything else that parses as a document is YAML. An
// extension is a claim about a file and this is a fact about it.
func OpenAPIMediaType(content []byte) string {
	var probe any
	if err := json.Unmarshal(content, &probe); err == nil {
		return MediaTypeOpenAPIJSON
	}
	return MediaTypeOpenAPIYAML
}
