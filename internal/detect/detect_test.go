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

package detect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// The two 404s. A path that is not in a repository and a repository that
// cannot be read are the same answer from every provider's API, and telling
// them apart is the difference between a message that fixes something and a
// message that sends somebody to correct a field that is already correct.

// blindReader is a repository nothing can list: every path is missing, which
// is what a repository the credential cannot see looks like from here. It
// answers no questions about the repository itself.
type blindReader struct{}

func (blindReader) ListDir(context.Context, string, string, string) ([]gitprovider.DirEntry, error) {
	return nil, fmt.Errorf("%w: the repository root", gitprovider.ErrFileNotFound)
}

func (blindReader) ReadFile(context.Context, string, string, string) ([]byte, error) {
	return nil, fmt.Errorf("%w", gitprovider.ErrFileNotFound)
}

// probingReader is a blindReader that can also be asked about the repository,
// and answers what it was built with.
type probingReader struct {
	blindReader
	err error
}

func (p probingReader) Repository(context.Context, string) (gitprovider.Repository, error) {
	if p.err != nil {
		return gitprovider.Repository{}, p.err
	}
	return gitprovider.Repository{FullName: "acme/shop", DefaultBranch: "main"}, nil
}

func target() Target {
	return Target{Repo: "acme/shop", Ref: "main", RootDirectory: "apps/shop", ConsiderDockerfile: true}
}

func TestARepositoryTheCredentialCannotSeeIsNotAMissingDirectory(t *testing.T) {
	reader := probingReader{err: fmt.Errorf("%w: acme/shop", gitprovider.ErrRepositoryNotFound)}

	_, err := Signals(context.Background(), reader, target())
	if !errors.Is(err, ErrRepositoryUnreadable) {
		t.Fatalf("an unreadable repository gave %v", err)
	}
	// The message it used to give described a directory, which is the one
	// thing this cannot be about: nothing was read.
	if strings.Contains(err.Error(), "directory") {
		t.Fatalf("an unreadable repository is still reported as a directory: %v", err)
	}
	if errors.Is(err, ErrNotRecognised) || errors.Is(err, ErrSourceUnreadable) {
		t.Fatalf("an unreadable repository is indistinguishable from the other two answers: %v", err)
	}
}

func TestAReadableRepositoryStillReportsAMissingDirectory(t *testing.T) {
	// The repository reads, so the path that did not is genuinely not in it —
	// which is what the preflight exists to catch, and the message that
	// fixes it.
	_, err := Signals(context.Background(), probingReader{}, target())
	if !errors.Is(err, ErrNotRecognised) {
		t.Fatalf("a wrong root directory gave %v", err)
	}
	if !strings.Contains(err.Error(), "apps/shop") {
		t.Fatalf("the answer does not name the directory that is missing: %v", err)
	}
}

func TestAProbeThatDoesNotAnswerSettlesNothing(t *testing.T) {
	// The repository may or may not be readable; the provider did not say.
	// That is worth another attempt rather than a verdict about a directory.
	reader := probingReader{err: errors.New("502 Bad Gateway")}

	_, err := Signals(context.Background(), reader, target())
	if !errors.Is(err, ErrSourceUnreadable) {
		t.Fatalf("a provider that would not answer gave %v", err)
	}
}

func TestAProviderThatCannotBeAskedKeepsItsOldAnswer(t *testing.T) {
	// A provider with no probe behind it is not a reason to invent a verdict:
	// the ambiguous message it always gave is better than a confident wrong
	// one.
	_, err := Signals(context.Background(), blindReader{}, target())
	if !errors.Is(err, ErrNotRecognised) {
		t.Fatalf("a provider that cannot be asked gave %v", err)
	}
}

func TestAskingAboutARepositoryOnItsOwn(t *testing.T) {
	// The callers with no listing to hang the question off — a preflight
	// whose branch resolution answered 404, a project's first build — ask
	// the same question and get a yes or a no.
	unreadable := probingReader{err: fmt.Errorf("%w: acme/shop", gitprovider.ErrRepositoryNotFound)}
	if !UnreadableRepository(context.Background(), unreadable, "acme/shop") {
		t.Fatal("a repository the credential cannot see answered readable")
	}
	if UnreadableRepository(context.Background(), probingReader{}, "acme/shop") {
		t.Fatal("a repository that reads answered unreadable")
	}
	// Neither a provider that cannot be asked nor one that did not answer has
	// established anything, and a verdict invented from silence is what this
	// exists to stop.
	if UnreadableRepository(context.Background(), blindReader{}, "acme/shop") {
		t.Fatal("a provider that cannot be asked was read as an answer")
	}
	if UnreadableRepository(context.Background(), probingReader{err: errors.New("502")}, "acme/shop") {
		t.Fatal("a provider that would not answer was read as an answer")
	}
}
