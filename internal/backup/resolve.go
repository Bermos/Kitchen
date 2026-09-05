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

package backup

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/backup/destination"
)

// The keys a destination's credential Secret holds. They are the object
// store's own names, because an S3 credential is an S3 credential wherever
// this platform stores one.
const (
	CredentialKeyAccessKeyID     = "accessKeyId"
	CredentialKeySecretAccessKey = "secretAccessKey"
)

// InternalCAFile is where every pod the platform writes sees the CA the
// operator mints for its own stores — the chart's `kitchen.internalCAFile`,
// mounted from the ConfigMap `kitchen-internal-ca`, in the operator's own pod
// and in the pod a scheduled backup run gets.
//
// It is a constant here rather than a field on the destination because
// nothing about it is configurable: there is one internal CA per platform
// namespace, and the only question is whether the destination is a store that
// it signed. TestTheCABundleIsMountedWhereTheChartSaysItIs holds this and the
// controller's mount path together.
const InternalCAFile = "/etc/kitchen/internal-ca/ca.crt"

// InCluster reports whether a destination's endpoint names a Service in this
// cluster rather than a store on the internet.
//
// It is a question about the certificate, not about the route: no public
// authority issues for a `.svc` name — nobody owns it — so a store answering
// TLS on one is served by an authority inside the cluster, and the only one
// this platform knows of is its own. Everything else, including a store on a
// private network with a real name, is verified against the host's roots as
// before.
func InCluster(endpoint string) bool {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "https" {
		// Plain HTTP verifies nothing, so there is no bundle that would
		// help; it is reported by the platform rather than fixed here.
		return false
	}
	host := parsed.Hostname()
	return strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local")
}

// Open builds the destination the spec describes, resolving its credential
// through the cluster.
//
// A destination naming no Secret is not an error and not a half-configuration:
// it is the ambient credential chain — IRSA, EKS Pod Identity, an instance
// role — which is the better answer where it is available, because there is
// then no long-lived key anywhere to leak.
func Open(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	spec *kitchenv1alpha1.BackupDestination,
) (destination.Destination, error) {
	if spec == nil {
		return nil, fmt.Errorf("this installation has no backup destination configured")
	}
	switch spec.Type {
	case kitchenv1alpha1.BackupDestinationS3:
		if spec.S3 == nil {
			// Admission refuses this, so reaching it means the object was
			// written before the rule existed or by something that bypassed
			// it. Say which half is missing rather than dereferencing nil.
			return nil, fmt.Errorf("the backup destination is of type s3 and carries no s3 block")
		}
		config := destination.S3Config{
			Bucket:               spec.S3.Bucket,
			Prefix:               spec.S3.Prefix,
			Region:               spec.S3.Region,
			Endpoint:             spec.S3.Endpoint,
			ForcePathStyle:       spec.S3.ForcePathStyle,
			ServerSideEncryption: spec.S3.ServerSideEncryption,
			KMSKeyID:             spec.S3.KMSKeyID,
		}
		if InCluster(config.Endpoint) {
			// A store inside this cluster over TLS: the platform's own CA is
			// the only authority that can have signed it, and this process —
			// the operator, or the pod of a scheduled run — mounts it.
			config.CABundleFile = InternalCAFile
		}
		if ref := spec.S3.CredentialsSecretRef; ref != nil {
			secret := &corev1.Secret{}
			key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
			if err := reader.Get(ctx, key, secret); err != nil {
				return nil, fmt.Errorf("the backup destination's credential %s/%s could not be read: %w",
					namespace, ref.Name, err)
			}
			config.AccessKeyID = string(secret.Data[CredentialKeyAccessKeyID])
			config.SecretAccessKey = string(secret.Data[CredentialKeySecretAccessKey])
			if config.AccessKeyID == "" || config.SecretAccessKey == "" {
				return nil, fmt.Errorf("the backup destination's credential %s/%s carries no %s and %s",
					namespace, ref.Name, CredentialKeyAccessKeyID, CredentialKeySecretAccessKey)
			}
		}
		return destination.NewS3(ctx, config)
	default:
		return nil, fmt.Errorf("this platform has no implementation for a %q backup destination", spec.Type)
	}
}

// Describe is a destination as a person reads it, and never its credential.
// It is what goes on status.backup.destination and into the API's view, and
// it answers without reaching the cluster — a description of where archives
// go should not depend on the store being up.
func Describe(spec *kitchenv1alpha1.BackupDestination) string {
	if spec == nil {
		return ""
	}
	if spec.Type == kitchenv1alpha1.BackupDestinationS3 && spec.S3 != nil {
		described := "s3://" + spec.S3.Bucket
		if spec.S3.Prefix != "" {
			described += "/" + spec.S3.Prefix
		}
		return described
	}
	return string(spec.Type)
}
