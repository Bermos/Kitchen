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
	"context"
	"fmt"
	"strconv"
	"strings"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The image's own `USER`, and the one deploy it has to refuse (#393).
//
// `runAsNonRoot` does not mean "run as somebody else". It means the kubelet
// *verifies*, before it creates the container, that the image does not run as
// uid 0 — and the only thing it can verify that against is a uid. `USER node`
// and `USER nonroot` are names, resolved inside the image against a passwd
// file the kubelet cannot read, so the guarantee is unenforceable and the
// container is refused:
//
//	container has runAsNonRoot and image has non-numeric user (node),
//	cannot verify user is non-root
//
// That is not an exotic image. It is what the official Node images document
// and what distroless ships. A project declaring the platform's own
// recommended posture, with an image that already honours it, does not start —
// and the only account of it was a container status on a pod.
//
// So the platform reads the user at the one moment it is already talking to
// the registry about this digest — a build's push, an acquisition's resolve,
// both of which pin the digest already (#306) — records it on the artifact,
// and refuses the *Release* where the combination cannot work. The refusal
// carries the image, the user it found, and the field that fixes it.
//
// What it deliberately does not do is refuse `runAsNonRoot` without
// `runAsUser` on sight. An image whose `USER` is `1001` satisfies the kubelet
// exactly as it stands, and a 400 for that project would be the platform
// refusing a request that would have worked.

// ImageUserReader reads the `USER` an image's config declares.
//
// It is an optional half of [ImageResolver] rather than a method on it: the
// resolver is faked in every test that acquires an image, and a fake that has
// no opinion about an image's user should go on having none. A resolver that
// does not implement this reports nothing, and nothing is exactly what an
// installation that cannot read the config knows.
type ImageUserReader interface {
	ImageUser(ctx context.Context, ref string) (string, error)
}

// imageUser is what the image says it runs as, or empty when the platform
// could not find out.
//
// Failing to read it is never fatal and never a build failure. The image
// exists, the deployment that follows is honest about what it is running, and
// what is lost is one check — which is the same bargain attestation makes when
// a registry cannot be reached.
func (r *BuildReconciler) imageUser(ctx context.Context, dockerConfig []byte, registry, ref string) string {
	log := logf.FromContext(ctx)
	factory := r.Resolvers
	if factory == nil {
		factory = defaultImageResolver
	}
	resolver, err := factory(dockerConfig, registry)
	if err != nil {
		log.V(1).Info("the image's user could not be read", "image", ref, "cause", err.Error())
		return ""
	}
	reader, ok := resolver.(ImageUserReader)
	if !ok {
		return ""
	}
	user, err := reader.ImageUser(ctx, ref)
	if err != nil {
		log.V(1).Info("the image's user could not be read", "image", ref, "cause", err.Error())
		return ""
	}
	return user
}

// imageUserIsName reports whether an image's recorded user is one the kubelet
// cannot verify: a name rather than a uid.
//
// The `USER` instruction takes `user`, `user:group`, `uid`, or `uid:gid`, and
// only the last two are numbers the kubelet can compare against zero. The
// group half is not looked at — the kubelet's check is about the user — and an
// empty user is not a name: an image that declares none runs as root, which is
// a different refusal with a different message and is the kubelet's to make.
func imageUserIsName(user string) bool {
	user = strings.TrimSpace(user)
	if user == "" {
		return false
	}
	if name, _, found := strings.Cut(user, ":"); found {
		user = name
	}
	_, err := strconv.ParseInt(user, 10, 64)
	return err != nil
}

// unverifiableImages are the artifacts of one unit that the posture their own
// workload runs under cannot be honoured on: `runAsNonRoot` asked for, no
// `runAsUser` named, and the image's own user a name.
//
// **The question is asked per workload, not per unit** (#399). Since a
// workload declares a posture of its own, merged over the unit's, the four
// images of one project can be under four different sets of constraints — so
// the check that fires for "the project" would either refuse a workload whose
// own `runAsUser` is exactly right or miss one whose inherited `runAsNonRoot`
// has no uid behind it. It is the same evidence #300 already records per
// artifact, read against the declaration that actually applies to it.
//
// It answers nothing at all in every other case, which is most of them — a
// posture that names a uid is enforceable whatever the image says, and an
// image whose user was never read is not evidence of anything.
func unverifiableImages(
	snapshot kitchenv1alpha1.ConfigSnapshot,
	artifacts []kitchenv1alpha1.BuildArtifact,
) []kitchenv1alpha1.BuildArtifact {
	var found []kitchenv1alpha1.BuildArtifact
	for _, artifact := range artifacts {
		security := workloadSecurity(snapshot, artifact.Workload)
		if security == nil || !security.RunAsNonRoot || security.RunAsUser > 0 {
			continue
		}
		if artifact.Artifact != nil && imageUserIsName(artifact.Artifact.User) {
			found = append(found, artifact)
		}
	}
	return found
}

// unverifiableImagesMessage is what such a build says, and it is written to be
// the last thing anybody needs to read about it: which workload, which image,
// the user the platform found in it, why the kubelet will not accept that, and
// the one field that fixes it.
func unverifiableImagesMessage(artifacts []kitchenv1alpha1.BuildArtifact) string {
	named := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		image := artifact.Artifact.Repository
		if artifact.Artifact.Digest != "" {
			image += "@" + artifact.Artifact.Digest
		}
		named = append(named, fmt.Sprintf("%s runs %s, whose USER is %q",
			artifact.Name(), image, artifact.Artifact.User))
	}
	return fmt.Sprintf(
		"runAsNonRoot is asked for with no runAsUser, and %s. The kubelet has to verify that "+
			"the image does not run as root before it starts the container, and it can only do that against a "+
			"uid — a name is resolved inside the image, where it cannot look — so that workload's pods would be "+
			"refused with \"cannot verify user is non-root\" and nothing would deploy. Set runAsUser to the uid "+
			"that user has in the image (1000 for the official Node images, 65532 for distroless) — on the "+
			"workload's own security where only it needs one, or on runtime.security for the whole unit — or "+
			"take runAsNonRoot off, and build again",
		strings.Join(named, "; "))
}

// reasonImageUserUnverifiable is a unit whose posture and whose images cannot
// both be honoured. It fails the Build for the same reason a bad kitchen.json
// does: nothing is wrong with the image, and no Release can be made from it as
// asked — so the refusal belongs where somebody is already looking, rather
// than on a pod nobody can see.
const reasonImageUserUnverifiable = "ImageUserUnverifiable"
