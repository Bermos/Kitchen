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

// Command backup writes a Kitchen backup archive from inside the cluster.
//
// The product surface for taking a backup is the dashboard's button, and this
// does not replace it: the same archive, written by the same code. What this
// exists for is the two cases the button cannot serve. A scheduled backup has
// no person to hold a token, so a CronJob of this is how an installation backs
// itself up every night. And a restore is only worth having if it has been
// run, which means CI has to be able to take an archive without an OAuth
// round trip — see docs/BACKUP.md and the "Back up, wipe and restore" step in
// .github/workflows/helm.yml.
//
// It ships in the operator's image for the same reason the restore command
// does: the archive and the code that reads it should be the same release.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/accountsdb"
	"github.com/Bermos/Kitchen/internal/backup"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "backup failed:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		output       string
		namespace    string
		skipAccounts bool
	)
	flag.StringVar(&output, "output", "",
		"Where to write the archive. \"-\" writes it to standard output.")
	flag.StringVar(&namespace, "namespace", controller.PlatformNamespace,
		"The platform namespace the objects and secrets are read from.")
	flag.BoolVar(&skipAccounts, "skip-accounts", false,
		"Leave the identity provider's database out of the archive.")
	flag.Parse()

	if output == "" {
		return errors.New("--output is required: where to write the archive")
	}
	ctx := ctrl.SetupSignalHandler()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	config, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("cannot reach the cluster: %w", err)
	}
	cluster, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	switch err := cluster.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); {
	case apierrors.IsNotFound(err):
		// An installation with no singleton is one the chart has not finished
		// installing. The objects are still worth taking; the manifest simply
		// names no installation.
	case err != nil:
		return err
	}

	exporter := &backup.Exporter{
		Client:      cluster,
		Namespace:   namespace,
		Version:     version.Version,
		ClusterName: kitchen.Spec.ClusterName,
		BaseDomain:  kitchen.Spec.BaseDomain,
	}
	if skipAccounts {
		exporter.AccountsMessage = "this backup was taken with --skip-accounts"
	} else {
		accounts, message := connectAccounts(ctx, cluster, namespace, kitchen)
		if accounts != nil {
			defer accounts.Close(ctx)
			exporter.Accounts = accounts
		} else {
			// Not fatal, and not silent. An installation with no identity
			// provider has no accounts to take; one whose database could not be
			// reached has accounts it is not backing up, and the difference has
			// to survive into the archive or it only surfaces at restore time.
			fmt.Fprintln(os.Stderr, "warning: no accounts in this archive:", message)
			exporter.AccountsMessage = message
		}
	}

	target := os.Stdout
	if output != "-" {
		// 0600: the archive is every credential the platform holds.
		file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("cannot write %s: %w", output, err)
		}
		defer func() { _ = file.Close() }()
		target = file
	}

	manifest, err := exporter.WriteTo(ctx, target)
	if err != nil {
		return err
	}
	report(manifest, output)
	return nil
}

// report goes to stderr when the archive is going to stdout, so that a piped
// backup carries the archive and nothing else.
func report(manifest backup.Manifest, output string) {
	out := os.Stdout
	if output == "-" {
		out = os.Stderr
	}
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format, args...) }
	say("wrote a backup of %s at %s (Kitchen %s)\n",
		manifest.BaseDomain, manifest.CreatedAt.Format(time.RFC3339), manifest.PlatformVersion)
	kinds := make([]string, 0, len(manifest.Resources))
	for plural := range manifest.Resources {
		kinds = append(kinds, plural)
	}
	sort.Strings(kinds)
	for _, plural := range kinds {
		if count := manifest.Resources[plural]; count > 0 {
			say("  %s: %d\n", plural, count)
		}
	}
	say("  secrets: %d\n", manifest.Secrets)
	if manifest.Accounts != nil {
		say("  accounts: %d rows across %d tables\n",
			manifest.Accounts.Rows, manifest.Accounts.Tables)
	}
}

// connectAccounts opens the identity provider's database, or says why it could
// not. Both halves of "could not" are real: an installation with no identity
// provider has no accounts to take, and one whose database is unreachable has
// accounts it is not backing up.
func connectAccounts(
	ctx context.Context,
	cluster client.Client,
	namespace string,
	kitchen *kitchenv1alpha1.Kitchen,
) (*accountsdb.Client, string) {
	if !kitchen.Spec.Auth.Enabled {
		return nil, "this installation has no identity provider, so there are no accounts to back up"
	}
	name := accountsdb.SecretName(kitchen)
	secret := &corev1.Secret{}
	if err := cluster.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, fmt.Sprintf("the accounts database's secret %s/%s could not be read: %s", namespace, name, err)
	}
	dsn, err := accountsdb.DSNFromSecret(secret)
	if err != nil {
		return nil, err.Error()
	}
	accounts, err := accountsdb.Connect(ctx, dsn)
	if err != nil {
		return nil, err.Error()
	}
	return accounts, ""
}
