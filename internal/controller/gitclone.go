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

package controller

import (
	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// cloneScript fetches exactly the commit under build, and nothing else: the
// history is not what is being built, and a shallow fetch of one revision is
// the cheapest thing a large repository can be asked for.
//
// The repository and the commit arrive as environment variables rather than
// substituted into the script. Both come out of a Project's spec, and nothing
// constrains a repository name to characters a shell reads literally.
//
// A private repository is cloned with the token mounted at
// KITCHEN_GIT_TOKEN_FILE, and git is told about it the one way that keeps the
// value out of everything that is written down: an askpass helper, which git
// runs with this process's environment and which reads the file itself. The
// URL keeps no credential, so `git remote -v` says what the pod spec says.
// GIT_TERMINAL_PROMPT=0 is what turns "wait forever for a username nobody can
// type" into an error, credential or not.
//
// KITCHEN_PRUNE_GIT_DIR removes the repository's own `.git` afterwards, which
// is what a BuildKit git context does: it hands the frontend a checkout with
// no git directory in it unless BUILDKIT_CONTEXT_KEEP_GIT_DIR asks for one. A
// clone that kept it would put the whole history into the context, where a
// `COPY . .` would put it into the image.
const cloneScript = `set -e
export GIT_TERMINAL_PROMPT=0
if [ -n "$KITCHEN_GIT_TOKEN_FILE" ]; then
	cat >"$KITCHEN_ASKPASS" <<'EOF'
#!/bin/sh
case "$1" in
Username*) printf 'x-access-token' ;;
*) cat "$KITCHEN_GIT_TOKEN_FILE" ;;
esac
EOF
	chmod 0700 "$KITCHEN_ASKPASS"
	export GIT_ASKPASS="$KITCHEN_ASKPASS"
fi
git init -q "$KITCHEN_SOURCE_DIR"
cd "$KITCHEN_SOURCE_DIR"
git remote add origin "$KITCHEN_GIT_URL"
git fetch -q --depth 1 origin "$KITCHEN_GIT_SHA"
git checkout -q FETCH_HEAD
if [ -n "$KITCHEN_PRUNE_GIT_DIR" ]; then
	rm -rf .git
fi`

// gitClone is the fetch, as one init container, and the only container in a
// build pod that ever holds the git credential. What comes after it reads a
// directory: the token is in a container that has already exited by the time
// anything out of the repository runs (#425).
type gitClone struct {
	// SourceDir is where the checkout lands and ScratchDir a writable
	// directory beside it — git's HOME, and where the askpass helper is
	// written. Both are on Mount, which is the volume the containers after
	// this one read the source from.
	SourceDir  string
	ScratchDir string
	Mount      corev1.VolumeMount

	// Secret is the Secret holding the token a private repository needs,
	// empty for a repository that needs none.
	Secret string

	// KeepGitDir leaves the repository's `.git` in the checkout. The
	// lifecycle is handed the directory a clone produces, `.git` and all,
	// and has been since buildpacks builds existed; a BuildKit context is
	// the checkout without it. See cloneScript.
	KeepGitDir bool

	// User is the uid to clone as, where the pod does not say. Whatever
	// reads the checkout afterwards runs as it too: buildpacks write into
	// the application directory, and BuildKit's client reads the context as
	// the unprivileged user its daemon runs as.
	User *int64
}

func (c gitClone) container(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "KITCHEN_SOURCE_DIR", Value: c.SourceDir},
		{Name: "KITCHEN_GIT_URL", Value: repoCloneURL(project)},
		{Name: "KITCHEN_GIT_SHA", Value: build.Spec.Git.SHA},
		// git wants a home to look for configuration in, and the
		// user it runs as here has none of its own.
		{Name: "HOME", Value: c.ScratchDir},
	}
	if !c.KeepGitDir {
		env = append(env, corev1.EnvVar{Name: "KITCHEN_PRUNE_GIT_DIR", Value: "1"})
	}
	mounts := []corev1.VolumeMount{c.Mount}
	if c.Secret != "" {
		env = append(env,
			corev1.EnvVar{Name: "KITCHEN_GIT_TOKEN_FILE", Value: gitCredentialFile},
			// The askpass helper is written beside the checkout, which is
			// the one directory this pod has that it can write to.
			corev1.EnvVar{Name: "KITCHEN_ASKPASS", Value: c.ScratchDir + "/askpass"},
		)
		mounts = append(mounts, gitCredentialMount())
	}
	container := corev1.Container{
		Name:         "clone",
		Image:        GitCloneImage,
		Command:      []string{"/bin/sh", "-c", cloneScript},
		Env:          env,
		VolumeMounts: mounts,
	}
	if c.User != nil {
		container.SecurityContext = &corev1.SecurityContext{RunAsUser: c.User}
	}
	return container
}
