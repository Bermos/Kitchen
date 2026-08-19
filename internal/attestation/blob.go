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

package attestation

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
)

// Handing bytes from a pod to the operator, without inventing a channel for it.
//
// A quality gate runs in a pod, in the application's namespace, as an image
// somebody else wrote. Its findings can be megabytes — a container image scan
// of a Node application routinely is — and every obvious way of getting them
// out is wrong at that size or in that place:
//
//   - a pod's termination message is capped at 4 KiB, which is why the build
//     digest fits there and a scan report does not;
//   - a ConfigMap or Secret caps at about a megabyte, and truncating findings
//     turns evidence into an opinion about which findings mattered;
//   - the pod's log is shipped by the collector, which is fast but not
//     synchronous, so reading it back races the Job finishing — and a race
//     that silently shortens evidence is the worst of the three.
//
// So the findings go where large content-addressed blobs already go: the
// registry the artifact itself is in, under the artifact's own repository,
// with the credential the pod already holds because it had to pull the image
// to scan it. The pod reports the digest, which is 71 bytes and fits anywhere,
// and the operator fetches exactly those bytes back.
//
// The blob is not evidence while it sits there — it is unsigned, and anything
// with push access could have written it. It becomes evidence when the
// operator reads it, wraps it in a statement and signs it. Until then it is a
// courier.

// PutBlob stores bytes in a repository and answers their digest.
//
// It is content-addressed, so storing the same findings twice stores them
// once, and a retry after a lost connection cannot produce two copies.
func (s *Store) PutBlob(ctx context.Context, repository string, body []byte) (string, error) {
	target, err := s.repository(repository)
	if err != nil {
		return "", err
	}
	layer := static.NewLayer(body, mediaTypeGateFindings)
	digest, err := layer.Digest()
	if err != nil {
		return "", err
	}
	if err := remote.WriteLayer(target, layer, s.options(ctx)...); err != nil {
		return "", fmt.Errorf("storing %d bytes in %s failed: %w", len(body), repository, err)
	}
	return digest.String(), nil
}

// Blob reads back what PutBlob stored.
//
// The registry checks the digest as it reads, so bytes that do not hash to the
// digest asked for are an error rather than a surprise later — which is the
// property that makes this safe as a courier for something about to be signed.
func (s *Store) Blob(ctx context.Context, repository, digest string) ([]byte, error) {
	target, err := s.repository(repository)
	if err != nil {
		return nil, err
	}
	hash, err := v1.NewHash(digest)
	if err != nil {
		return nil, fmt.Errorf("%q is not a digest: %w", digest, err)
	}
	layer, err := remote.Layer(target.Digest(hash.String()), s.options(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("reading %s from %s failed: %w", digest, repository, err)
	}
	reader, err := layer.Compressed()
	if err != nil {
		return nil, fmt.Errorf("reading %s from %s failed: %w", digest, repository, err)
	}
	defer func() { _ = reader.Close() }()

	body := &bytes.Buffer{}
	if _, err := io.Copy(body, io.LimitReader(reader, maxFindingsBytes+1)); err != nil {
		return nil, fmt.Errorf("reading %s from %s failed: %w", digest, repository, err)
	}
	if body.Len() > maxFindingsBytes {
		return nil, fmt.Errorf(
			"the findings at %s are larger than %d bytes, which is more than an attestation should carry",
			digest, maxFindingsBytes)
	}
	return body.Bytes(), nil
}

const (
	// mediaTypeGateFindings is what a gate's raw output is stored as. It is
	// deliberately not a layer media type any image tooling will try to
	// unpack: this is a blob being couriered, not part of an image.
	mediaTypeGateFindings = "application/vnd.kitchen.gate-findings"

	// maxFindingsBytes bounds what will be pulled back and signed.
	//
	// The limit exists because the operator holds the whole thing in memory to
	// hash and sign it, and because an attestation nothing can retrieve is not
	// useful evidence. Sixteen megabytes is far past any real scan report and
	// far short of anything that would trouble the process.
	maxFindingsBytes = 16 << 20
)

// repository parses a repository reference, honouring the plain-HTTP setting
// tests run under.
func (s *Store) repository(repository string) (name.Repository, error) {
	options := []name.Option{}
	if s.PlainHTTP {
		options = append(options, name.Insecure)
	}
	target, err := name.NewRepository(repository, options...)
	if err != nil {
		return name.Repository{}, fmt.Errorf("%q is not a repository: %w", repository, err)
	}
	return target, nil
}
