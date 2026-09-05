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

package accountsdb

import (
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Secret keys the chart writes into the accounts database's secret. The auth
// service reads `dsn` and nothing else; the pieces are here because a secret
// somebody wrote by hand for an external Postgres may carry those instead.
const (
	SecretKeyDSN      = "dsn"
	SecretKeyHost     = "host"
	SecretKeyPort     = "port"
	SecretKeyDatabase = "database"
	SecretKeyUsername = "username"
	SecretKeyPassword = "password"

	// SecretKeySSLMode is what every client asks of the connection, and
	// SecretKeyCAFile what it verifies the server against. Both are already
	// in the DSN the chart writes; they are keys of their own because a
	// secret somebody wrote by hand for an external Postgres may carry the
	// pieces instead, and because the platform reports on whether its own
	// accounts database is reached in the clear.
	SecretKeySSLMode = "sslmode"
	SecretKeyCAFile  = "caFile"

	// SecretKeyCertificateSecret is addressed to the operator rather than to
	// any client: it names the Secret the database's certificate belongs in,
	// and by naming it asks InternalTLSReconciler to issue one from the
	// platform's internal CA. It is the same key, spelled the same way, as
	// the telemetry store's connection secret carries — one vocabulary, so
	// one controller serves both.
	SecretKeyCertificateSecret = "certificateSecret"
)

// SSLModeVerifyFull is what the chart asks of the bundled database, and the
// only mode that means the same thing to both drivers in front of it: the
// operator's is libpq's, where `require` encrypts without verifying, and the
// identity provider's is node-postgres, where it verifies.
const SSLModeVerifyFull = "verify-full"

// DefaultSecretName is the accounts database's secret on an installation whose
// Kitchen object does not name one.
//
// The Kitchen singleton is applied as a post-install hook and is not
// re-applied on upgrade, so an installation that predates
// `spec.auth.databaseSecretRef` would otherwise have no accounts in its
// backups and no explanation for it. Every chart-generated name is release-name
// prefixed and the conventional release name is `kitchen`, which is the same
// fallback the bundled registry's credential uses.
const DefaultSecretName = "kitchen-postgres"

// SecretName is where the connection to the accounts database is kept.
func SecretName(kitchen *kitchenv1alpha1.Kitchen) string {
	if kitchen != nil && kitchen.Spec.Auth.DatabaseSecretRef != nil &&
		kitchen.Spec.Auth.DatabaseSecretRef.Name != "" {
		return kitchen.Spec.Auth.DatabaseSecretRef.Name
	}
	return DefaultSecretName
}

// DSNFromSecret reads the connection string out of the secret the chart
// writes, assembling one from the pieces where the secret carries those
// instead.
func DSNFromSecret(secret *corev1.Secret) (string, error) {
	if dsn := strings.TrimSpace(string(secret.Data[SecretKeyDSN])); dsn != "" {
		return dsn, nil
	}

	value := func(key string) string { return strings.TrimSpace(string(secret.Data[key])) }
	host, database := value(SecretKeyHost), value(SecretKeyDatabase)
	username, password := value(SecretKeyUsername), value(SecretKeyPassword)

	var missing []string
	for key, present := range map[string]string{
		SecretKeyHost: host, SecretKeyDatabase: database, SecretKeyUsername: username,
	} {
		if present == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("the secret %s/%s carries no %s, and none of %s to build one from",
			secret.Namespace, secret.Name, SecretKeyDSN, strings.Join(sorted(missing), ", "))
	}

	port := value(SecretKeyPort)
	if port == "" {
		port = "5432"
	}
	dsn := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(username, password),
		Host:   host + ":" + port,
		Path:   "/" + database,
	}
	// What the connection asks of the server, where the secret says. Both are
	// libpq's own spelling, which is what pgx reads here — and what the chart
	// puts in the `dsn` key above, so a secret assembled from pieces cannot
	// end up less encrypted than the one the chart writes.
	query := url.Values{}
	if sslmode := value(SecretKeySSLMode); sslmode != "" {
		query.Set("sslmode", sslmode)
	}
	if caFile := value(SecretKeyCAFile); caFile != "" {
		query.Set("sslrootcert", caFile)
	}
	dsn.RawQuery = query.Encode()
	return dsn.String(), nil
}

// sorted keeps the missing-key message stable, since it is built from a map.
func sorted(values []string) []string {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
	return values
}
