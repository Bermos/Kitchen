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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// Making a rotated credential reach what is already running.
//
// A container reads its environment once, at exec, and never again — so
// replacing a value under an unchanged name changes nothing about a pod that
// is already up. Rotation is a first-class operation here (setting and
// rotating a project secret are the same request, and a claim's provider can
// replace a binding's credential), which made this the worst-shaped defect
// there is: the write succeeded, the audit log recorded a credential change,
// and the application went on using the old value indefinitely.
//
// The mechanism is the usual one — a digest of the Secrets a workload reads,
// on its pod template, so the Deployment rolls when and only when the content
// it reads changes. The half that is actually work is *which* workloads
// consume a given Secret, because a Secret reaches a pod by more than one
// path: an environment variable naming one key, `envFrom` taking every key,
// and a file mounted from one. secretsReadBy walks all of them, off the pod
// spec the reconciler has just built, so a path added later is covered by the
// one read rather than by remembering this file exists.

const (
	// SecretsRevisionAnnotation carries a digest of the Secrets one workload
	// actually reads. It is on the pod template, so rotating a value rolls
	// the pods that read it and leaves every other workload alone.
	//
	// Without it a rotation reaches an application whenever a pod happens to
	// restart — some pods on the old value and some on the new, for as long
	// as nothing redeploys. That is a worse answer than "on the next deploy",
	// because it is not an answer at all.
	SecretsRevisionAnnotation = "kitchen.bermos.dev/secrets-revision"

	// projectSecretsRevisionAnnotation is what the digest was called while it
	// covered the project's own secrets and nothing else. It is deleted from
	// every template this writes, so that an installation upgrading into the
	// wider digest does not carry two answers to one question. That upgrade
	// rolls each workload once, because the digest is over more than it was.
	projectSecretsRevisionAnnotation = "kitchen.bermos.dev/project-secrets-revision"
)

// secretRotation is what one workload's digest did on one reconcile: the
// value the pod template carried, the value it carries now, and the Secrets
// the digest was taken over.
//
// It exists to make the restart explicable. A pod roll with no account of
// itself is indistinguishable from the platform having decided something on
// its own, which is precisely the thing an operator watching a rotation land
// needs to be able to tell apart.
type secretRotation struct {
	from    string
	to      string
	secrets []string
}

// rolled says this reconcile replaced one digest with another, which is a
// rotation and nothing else.
//
// A workload seen for the first time has no previous digest, and one that
// stops reading secrets loses its digest with the reference — neither is a
// value changing under a running pod, and both already come with the deploy
// that caused them.
func (rotation secretRotation) rolled() bool {
	return rotation.from != "" && rotation.to != "" && rotation.from != rotation.to
}

// cause names what changed, as far as a digest can: the Secret, where the
// workload reads one, or the set it reads where it reads several.
func (rotation secretRotation) cause() string {
	if len(rotation.secrets) == 1 {
		return rotation.secrets[0] + " was rotated"
	}
	return "one of the secrets it reads was rotated (" + strings.Join(rotation.secrets, ", ") + ")"
}

// secretsReadBy lists the Secrets a pod spec reads and, for each, which of
// its keys. A nil key list means the whole Secret — `envFrom` takes every key
// there is, and so does a volume mounted without an item list, so both have
// to be digested whole.
//
// `imagePullSecrets` is deliberately not among them. The kubelet reads the
// pull credential at pull time rather than handing it to the process, so a
// rotated registry password is in use the moment it is written and a running
// pod holds nothing stale. Rolling on it would be a restart that changes
// nothing — and on a workload deployed by recreation, an outage that changes
// nothing.
func secretsReadBy(spec *corev1.PodSpec) map[string][]string {
	whole := map[string]bool{}
	keyed := map[string]map[string]bool{}
	readWhole := func(name string) {
		if name != "" {
			whole[name] = true
		}
	}
	readKey := func(name, key string) {
		if name == "" {
			return
		}
		if keyed[name] == nil {
			keyed[name] = map[string]bool{}
		}
		keyed[name][key] = true
	}

	containers := make([]corev1.Container, 0, len(spec.InitContainers)+len(spec.Containers))
	containers = append(containers, spec.InitContainers...)
	containers = append(containers, spec.Containers...)
	for i := range containers {
		for _, variable := range containers[i].Env {
			if ref := variable.ValueFrom; ref != nil && ref.SecretKeyRef != nil {
				readKey(ref.SecretKeyRef.Name, ref.SecretKeyRef.Key)
			}
		}
		for _, source := range containers[i].EnvFrom {
			if source.SecretRef != nil {
				readWhole(source.SecretRef.Name)
			}
		}
	}
	for _, volume := range spec.Volumes {
		switch {
		case volume.Secret != nil:
			if len(volume.Secret.Items) == 0 {
				readWhole(volume.Secret.SecretName)
				continue
			}
			for _, item := range volume.Secret.Items {
				readKey(volume.Secret.SecretName, item.Key)
			}
		case volume.Projected != nil:
			for _, source := range volume.Projected.Sources {
				if source.Secret == nil {
					continue
				}
				if len(source.Secret.Items) == 0 {
					readWhole(source.Secret.Name)
					continue
				}
				for _, item := range source.Secret.Items {
					readKey(source.Secret.Name, item.Key)
				}
			}
		}
	}

	read := make(map[string][]string, len(whole)+len(keyed))
	for name := range whole {
		read[name] = nil
	}
	for name, set := range keyed {
		if whole[name] {
			continue
		}
		keys := make([]string, 0, len(set))
		for key := range set {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		read[name] = keys
	}
	return read
}

// secretsRevision digests the content a pod spec reads out of the Secrets it
// names, and answers "" for a workload that reads none. It answers the names
// it digested as well, sorted, which is what the activity entry says the
// restart was about.
//
// Only the referenced keys are hashed, so adding an unrelated value to a
// Secret several workloads share does not roll all of them — the digest moves
// for the workloads whose values moved and for no others.
func secretsRevision(
	ctx context.Context,
	c client.Client,
	namespace string,
	spec *corev1.PodSpec,
) (string, []string, error) {
	read := secretsReadBy(spec)
	if len(read) == 0 {
		return "", nil, nil
	}
	names := make([]string, 0, len(read))
	for name := range read {
		names = append(names, name)
	}
	sort.Strings(names)

	digest := sha256.New()
	digested := make([]string, 0, len(names))
	for _, name := range names {
		secret := &corev1.Secret{}
		switch err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); {
		case apierrors.IsNotFound(err):
			// A reference to a Secret that is not there yet. The container
			// will not start until it is, and the reconcile that creates it
			// is what brings the digest back.
			continue
		case err != nil:
			return "", nil, err
		}
		keys := read[name]
		if keys == nil {
			keys = make([]string, 0, len(secret.Data))
			for key := range secret.Data {
				keys = append(keys, key)
			}
			sort.Strings(keys)
		}
		digested = append(digested, name)
		// The name goes into the digest with the values, so that moving a
		// value from one Secret to another is a change even where the bytes
		// are the same.
		digest.Write([]byte(name))
		digest.Write([]byte{0})
		for _, key := range keys {
			digest.Write([]byte(key))
			digest.Write([]byte{0})
			digest.Write(secret.Data[key])
			digest.Write([]byte{0})
		}
	}
	if len(digested) == 0 {
		return "", nil, nil
	}
	return hex.EncodeToString(digest.Sum(nil))[:16], digested, nil
}

// stampSecretsRevision digests what a pod template reads, stamps the template
// with the answer, and reports what that did to the digest it was carrying.
//
// It is called with the template the mutation has just finished building, so
// that every path a Secret reaches the pod by is covered by one read of one
// object rather than by three lists kept in step with it.
func stampSecretsRevision(
	ctx context.Context,
	c client.Client,
	namespace string,
	template *corev1.PodTemplateSpec,
) (secretRotation, error) {
	revision, names, err := secretsRevision(ctx, c, namespace, &template.Spec)
	if err != nil {
		return secretRotation{}, err
	}
	rotation := secretRotation{
		from:    template.Annotations[SecretsRevisionAnnotation],
		to:      revision,
		secrets: names,
	}
	applySecretsRevision(&template.ObjectMeta, revision)
	return rotation, nil
}

// applySecretsRevision stamps a pod template with the digest, or takes the
// stamp off a template that no longer reads any Secret.
func applySecretsRevision(template *metav1.ObjectMeta, revision string) {
	// The key this used to be written under, removed whatever happens: two
	// digests of the same question on one template is a question about which
	// of them is current.
	delete(template.Annotations, projectSecretsRevisionAnnotation)
	if revision == "" {
		delete(template.Annotations, SecretsRevisionAnnotation)
		return
	}
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[SecretsRevisionAnnotation] = revision
}

// announceRotation puts the restart in the activity feed, with its cause.
//
// A rolled workload is otherwise an unexplained roll: the pods of one
// environment restart, at a moment nobody deployed anything, and the only
// record of why is a digest on a pod template. The feed is where the platform
// accounts for what it did on its own, so the entry says which workload, what
// it was reading, and — where the workload runs one at a time — that the
// restart is a gap rather than a rollout.
func (r *EnvironmentReconciler) announceRotation(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	// process names the worker this is about, and is empty for the
	// environment's web process.
	process string,
	rotation secretRotation,
	// interrupts says the workload is deployed by recreation, so the old pod
	// stops before the new one starts.
	interrupts bool,
) {
	if !rotation.rolled() {
		return
	}
	workload := "the web process"
	if process != "" {
		workload = fmt.Sprintf("worker %q", process)
	}
	message := fmt.Sprintf("restarting %s of %s: %s", workload, env.Name, rotation.cause())
	if interrupts {
		message += "; it runs one at a time, so the old pod stops before the new one starts"
	}
	r.Activity.Record(ctx, clickhouse.Event{
		Type:        clickhouse.EventSecretRotated,
		Project:     env.Spec.ProjectRef.Name,
		Environment: env.Name,
		Process:     process,
		Message:     message,
	})
}
