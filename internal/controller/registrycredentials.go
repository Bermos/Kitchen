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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Which credential a pod that touches the registry is given (#424).
//
// A registry Connection's credential is per connection, not per project: on
// the bundled registry it is one account that can push over any tag of any
// project's repository. Several pods the platform runs contain code the
// platform did not write — the Cloud Native Buildpacks lifecycle runs the
// repository's own build, a quality gate and a vulnerability scanner are
// images an operator chose — and reading a docker config off the filesystem
// is a line in a `postinstall` script.
//
// So a credential is given to the container that needs it and to no other,
// and where the registry can issue one that cannot push, that is the one
// third-party code gets. Both halves matter: splitting the containers alone
// would still hand the exporter's credential to a scanner, and a read-only
// credential alone would still leave it in the container running the build.

// readCredentialSuffix names the read-only credential beside the credential
// it is narrower than. It is a convention rather than a field so that an
// installation pointing at a registry of its own can supply one: a Harbor
// robot account with pull rights, stored as `<credentials secret>-read` in
// the platform namespace, is picked up with no further configuration.
const readCredentialSuffix = "-read"

// readCredentialSecretName is the read-only credential that goes with one
// credential Secret, wherever that Secret lives — the platform namespace for
// a Connection's own, an application namespace for the copy a build syncs.
func readCredentialSecretName(credentialsSecret string) string {
	if credentialsSecret == "" {
		return ""
	}
	return credentialsSecret + readCredentialSuffix
}

// registryCredentialsForPod is what one pod is given: the Connection's own
// credential for the container that pushes, and a credential that cannot push
// for the containers running code the platform did not write.
//
// Read is never empty when Push is not. An installation whose registry issues
// no scoped credential gets the same name in both, which is the fallback said
// out loud rather than a pod with no credential at all — the Connection's
// status is where it is reported (see connection_controller.go).
type registryCredentialsForPod struct {
	// Push is the credential the platform's own containers use: the
	// lifecycle's export phase, a gate's publisher.
	Push string
	// Read is what everything else in the pod gets.
	Read string
}

// scoped reports whether the two are actually different — whether this
// installation's registry issued a credential narrower than the Connection's.
func (c registryCredentialsForPod) scoped() bool {
	return c.Push != "" && c.Read != c.Push
}

// credentialsWithRead pairs a credential with the read-only one beside it,
// falling back to the credential itself when there is none.
func credentialsWithRead(push, read string) registryCredentialsForPod {
	if read == "" {
		return registryCredentialsForPod{Push: push, Read: push}
	}
	return registryCredentialsForPod{Push: push, Read: read}
}

// resolveRegistryCredentials answers the same question for a pod created
// after the build that pushed the artifact — a gate, a scan, a bill of
// materials — where nothing has just synced the credentials and the copies
// are already in the application namespace, or are not.
//
// A read-only credential that is not there is not a fault: it is a vendored
// image pulled from a registry the platform holds no scoped account at, or an
// installation that upgraded after the artifact was built. Both read the
// artifact with the Connection's own credential, which is what they did
// before.
func resolveRegistryCredentials(
	ctx context.Context, c client.Client, namespace, pushSecret string,
) registryCredentialsForPod {
	if pushSecret == "" {
		return registryCredentialsForPod{}
	}
	read := readCredentialSecretName(pushSecret)
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: read}, &corev1.Secret{}); err != nil {
		return credentialsWithRead(pushSecret, "")
	}
	return credentialsWithRead(pushSecret, read)
}
