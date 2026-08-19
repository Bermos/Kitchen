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

// Command qualitygate carries a quality gate's findings out of the pod that
// produced them.
//
// It is not the gate. The gate is an image somebody else wrote — Trivy, a SAST
// runner, a test suite — which runs first, as an init container, and writes
// what it found to a file on a shared volume. This runs second, reads that
// file, stores it in the registry the artifact is in, and reports the digest
// through the pod's termination message, which the operator reads off the pod
// status.
//
// It exists because there is no good way to hand a megabyte out of a finished
// pod. The termination message caps at 4 KiB; a ConfigMap caps at about a
// megabyte and truncating findings turns evidence into an opinion; the pod's
// log is shipped asynchronously and reading it back races the Job finishing.
// The registry has none of those limits and the pod already holds a credential
// for it, because it had to pull the artifact to scan it.
//
// What it stores is not evidence yet. It is unsigned, and anything with push
// access could have written it — it becomes evidence when the operator reads
// it back, wraps it in a statement and signs it under the platform's key,
// which is a key that never comes near this pod. See docs/COMPLIANCE.md.
//
// It ships in the operator's image and is never run by a person.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Bermos/Kitchen/internal/attestation"
)

// The pod's contract with the operator, in both directions.
const (
	// envArtifact is the artifact being gated, as repository@digest. The
	// findings are stored in that artifact's own repository.
	envArtifact = "KITCHEN_ARTIFACT"

	// envFindings is the file the gate was told to write.
	envFindings = "KITCHEN_FINDINGS"

	// envGate is the gate's name, echoed back so that a report cannot be
	// mistaken for another gate's.
	envGate = "KITCHEN_GATE"

	// envTerminationLog is where to write the report the operator reads. It
	// is passed rather than compiled in so that the two ends cannot disagree
	// about it silently.
	envTerminationLog = "KITCHEN_TERMINATION_LOG"

	// envDockerConfig is the directory holding the registry credential, in
	// docker's own environment variable so that the gate image beside this
	// one reads the same one without being told.
	envDockerConfig = "DOCKER_CONFIG"
)

// report is what the operator reads off the pod. It is small on purpose: the
// findings are in the registry and this says where.
type report struct {
	Gate string `json:"gate"`
	// Blob is the digest the findings are stored under, in the artifact's
	// repository.
	Blob string `json:"blob"`
	// Bytes is how large they were, so that a gate which wrote nothing is
	// distinguishable from one whose output went missing.
	Bytes int `json:"bytes"`
	// FinishedAt is when the gate's output was read, which is as close to
	// when the gate finished as this process can honestly claim.
	FinishedAt string `json:"finishedAt"`
	// Error explains a run whose findings could not be stored. The operator
	// treats a report carrying one as a gate that did not produce evidence.
	Error string `json:"error,omitempty"`
}

func main() {
	if err := run(context.Background()); err != nil {
		// The failure is written to the termination message as well as to
		// stderr, because stderr is a log line the operator would have to go
		// looking for and the termination message is on the pod status it is
		// already reading.
		// If even this cannot be written the process is out of options; the
		// operator will see a gate that finished and left no report, which is
		// the same conclusion by a longer route.
		_ = writeTermination(report{
			Gate:       os.Getenv(envGate),
			Error:      err.Error(),
			FinishedAt: time.Now().UTC().Format(time.RFC3339),
		})
		fmt.Fprintf(os.Stderr, "publishing the gate's findings failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	artifact := os.Getenv(envArtifact)
	findingsPath := os.Getenv(envFindings)
	gate := os.Getenv(envGate)
	if artifact == "" || findingsPath == "" || gate == "" {
		return fmt.Errorf("%s, %s and %s must all be set", envArtifact, envFindings, envGate)
	}

	body, err := os.ReadFile(filepath.Clean(findingsPath))
	if err != nil {
		// A gate that exited cleanly and wrote nothing has not run, whatever
		// its exit code said. Saying so is the point: a gate silently
		// producing no findings reads exactly like a clean scan.
		return fmt.Errorf("the gate wrote no findings to %s: %w", findingsPath, err)
	}
	if len(body) == 0 {
		return fmt.Errorf("the gate wrote an empty file to %s", findingsPath)
	}

	repository, _, found := strings.Cut(artifact, "@")
	if !found {
		return fmt.Errorf(
			"the artifact must be given as repository@digest, and %q is not — "+
				"a gate result about a tag is a result about a moving target",
			artifact)
	}

	store, err := storeFor(repository)
	if err != nil {
		return err
	}
	digest, err := store.PutBlob(ctx, repository, body)
	if err != nil {
		return err
	}

	return writeTermination(report{
		Gate:       gate,
		Blob:       digest,
		Bytes:      len(body),
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// storeFor builds a registry client from the credential mounted beside this
// process — the same one the gate pulled the artifact with.
func storeFor(repository string) (*attestation.Store, error) {
	directory := os.Getenv(envDockerConfig)
	if directory == "" {
		return nil, fmt.Errorf("%s is not set, so there is no credential to store the findings with", envDockerConfig)
	}
	config, err := os.ReadFile(filepath.Clean(filepath.Join(directory, "config.json")))
	if err != nil {
		return nil, fmt.Errorf("the registry credential could not be read: %w", err)
	}
	server, _, _ := strings.Cut(repository, "/")
	auth, err := attestation.AuthFromDockerConfig(config, server)
	if err != nil {
		return nil, err
	}
	return &attestation.Store{Auth: auth}, nil
}

// writeTermination puts the report where the operator reads it.
func writeTermination(answer report) error {
	path := os.Getenv(envTerminationLog)
	if path == "" {
		path = "/dev/termination-log"
	}
	body, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Clean(path), body, 0o600)
}
