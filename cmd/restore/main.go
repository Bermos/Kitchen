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

// Command restore puts a Kitchen backup archive back into a freshly installed
// platform: every custom resource, the credentials in the platform namespace,
// and the identity provider's database.
//
// It is a command rather than an API endpoint because of who is left to run
// it. A restore happens into a cluster whose accounts database is gone, and
// the credentials to authenticate against the REST API are inside the archive
// — so there is nobody to press a button. That puts it in the same category as
// installing the chart and following the bootstrap link: cluster bootstrap,
// which is the exception CLAUDE.md keeps to "nothing needs kubectl". Nobody
// runs this by hand either: the chart renders a Job for it, and this ships in
// the operator's image so that the archive and the code that reads it are the
// same release.
//
// The order it expects is the order docs/BACKUP.md describes. Install the
// chart at the release the archive was written by, wait for the identity
// provider to be ready — it migrates its own schema, and the accounts dump
// carries rows and not a schema — then run this.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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

// restartedAtAnnotation is what a rolled workload is stamped with, so that a
// person reading the Deployment afterwards finds out why it restarted.
const restartedAtAnnotation = "kitchen.bermos.dev/restored-at"

// authComponentLabel selects the identity provider's Deployment. Every
// chart-generated name is release-name prefixed, so it is found by what it is
// rather than by what it is called — the same reason the component survey
// selects on a label.
const authComponentLabel = "app.kubernetes.io/component"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "restore failed:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		archivePath   string
		namespace     string
		force         bool
		skipAccounts  bool
		restartAuth   bool
		waitForSchema time.Duration
	)
	flag.StringVar(&archivePath, "archive", "",
		"Path to the backup archive. \"-\" reads it from standard input.")
	flag.StringVar(&namespace, "namespace", controller.PlatformNamespace,
		"The platform namespace the objects and secrets are restored into.")
	flag.BoolVar(&force, "force", false,
		"Restore an archive written by a different release. The accounts dump carries rows and not a schema, "+
			"so it only fits the schema its own release migrates into place.")
	flag.BoolVar(&skipAccounts, "skip-accounts", false,
		"Restore the cluster half alone and leave the identity provider's database as it is.")
	flag.BoolVar(&restartAuth, "restart-auth", true,
		"Roll the identity provider after restoring, so that it picks up the signing secret from the archive. "+
			"Without it every restored session and API key stays unreadable until something else restarts it.")
	flag.DurationVar(&waitForSchema, "wait-for-schema", 5*time.Minute,
		"How long to wait for the identity provider to have migrated its schema. The accounts dump is data "+
			"only, so there has to be a schema to put it in.")
	flag.Parse()

	if archivePath == "" {
		return errors.New("--archive is required: the path to the backup to restore")
	}

	ctx := ctrl.SetupSignalHandler()

	source := os.Stdin
	if archivePath != "-" {
		file, err := os.Open(archivePath)
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", archivePath, err)
		}
		defer func() { _ = file.Close() }()
		source = file
	}
	archive, err := backup.Read(source)
	if err != nil {
		return err
	}
	describe(archive)

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

	restorer := &backup.Restorer{
		Client:    cluster,
		Namespace: namespace,
		Version:   version.Version,
		Force:     force,
	}

	if archive.Accounts != nil && !skipAccounts {
		// The connection comes off the *live* installation rather than off the
		// archive: the database being restored into is the one this install
		// just created, and its credential is the one the chart generated for
		// it. That is also why the archive's own copy of that secret is never
		// written back — see backup.Restorer.
		accounts, err := connectAccounts(ctx, cluster, namespace, waitForSchema)
		if err != nil {
			return err
		}
		defer accounts.Close(ctx)
		restorer.Accounts = accounts
	}

	report, err := restorer.Restore(ctx, archive)
	printReport(report)
	if err != nil {
		return err
	}

	if restartAuth && restorer.Accounts != nil {
		if err := rollIdentityProvider(ctx, cluster, namespace); err != nil {
			// Not fatal: everything is restored, and an identity provider that
			// was not rolled is one somebody can roll. Saying so is what stops
			// it becoming a mystery an hour later.
			fmt.Printf("the identity provider was not restarted (%s); restart it so it reads the restored "+
				"signing secret\n", err)
		} else {
			fmt.Println("the identity provider was restarted, so it reads the restored signing secret")
		}
	}
	fmt.Println("restore complete")
	return nil
}

// describe prints what the archive is, before anything is written. A person
// watching a Job's logs should be able to tell it is restoring the right
// platform from the first lines.
func describe(archive *backup.Archive) {
	manifest := archive.Manifest
	fmt.Printf("archive from %s, written by Kitchen %s at %s\n",
		installationName(manifest), manifest.PlatformVersion, manifest.CreatedAt.Format(time.RFC3339))
	kinds := make([]string, 0, len(manifest.Resources))
	for plural, count := range manifest.Resources {
		if count > 0 {
			kinds = append(kinds, fmt.Sprintf("%d %s", count, plural))
		}
	}
	sort.Strings(kinds)
	fmt.Printf("  objects: %v\n", kinds)
	fmt.Printf("  secrets: %d\n", manifest.Secrets)
	switch {
	case manifest.Accounts != nil:
		fmt.Printf("  accounts: %d rows across %d tables of %s\n",
			manifest.Accounts.Rows, manifest.Accounts.Tables, manifest.Accounts.Database)
	default:
		fmt.Printf("  accounts: none — %s\n", manifest.AccountsMessage)
	}
}

func installationName(manifest backup.Manifest) string {
	switch {
	case manifest.ClusterName != "":
		return manifest.ClusterName
	case manifest.BaseDomain != "":
		return manifest.BaseDomain
	default:
		return "an unnamed installation"
	}
}

func printReport(report backup.Report) {
	for plural, count := range report.Created {
		fmt.Printf("created %d %s\n", count, plural)
	}
	for plural, count := range report.Updated {
		fmt.Printf("updated %d %s\n", count, plural)
	}
	if report.Secrets > 0 {
		fmt.Printf("restored %d secrets\n", report.Secrets)
	}
	for _, skipped := range report.SecretsSkipped {
		fmt.Printf("left alone: %s\n", skipped)
	}
	if report.AccountsRows > 0 {
		fmt.Printf("restored %d rows into the identity provider's database\n", report.AccountsRows)
	}
	if report.AccountsMessage != "" {
		fmt.Println(report.AccountsMessage)
	}
	for _, warning := range report.Warnings {
		fmt.Println("warning:", warning)
	}
}

// connectAccounts opens the identity provider's database and waits for it to
// have a schema.
//
// The wait is the whole reason this is not one line. The chart starts the
// identity provider and this Job at the same time, and the identity provider
// is what creates the tables (auth/src/db.ts migrates on every start), so a
// restore that connected and copied immediately would find an empty database
// and fail on the first table.
func connectAccounts(
	ctx context.Context,
	cluster client.Client,
	namespace string,
	wait time.Duration,
) (*accountsdb.Client, error) {
	name := accountsdb.DefaultSecretName
	kitchen := &kitchenv1alpha1.Kitchen{}
	switch err := cluster.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); {
	case err == nil:
		name = accountsdb.SecretName(kitchen)
	case !apierrors.IsNotFound(err):
		return nil, err
	}

	secret := &corev1.Secret{}
	if err := cluster.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("cannot read the accounts database's secret %s/%s: %w. Restore with "+
			"--skip-accounts to put the cluster half back without it", namespace, name, err)
	}
	dsn, err := accountsdb.DSNFromSecret(secret)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(wait)
	for {
		accounts, err := accountsdb.Connect(ctx, dsn)
		if err == nil {
			return accounts, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the accounts database was not reachable within %s: %w", wait, err)
		}
		fmt.Println("waiting for the accounts database:", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// rollIdentityProvider restarts the identity provider so that it reads the
// signing secret the restore just put back.
//
// It matters more than it looks: `secret` in the identity provider's own
// credential is what every session and API key in the restored database was
// signed with, and the pods that are running came up with the one the fresh
// install generated. Until they restart, every restored login is refused by a
// platform that otherwise looks entirely healthy.
func rollIdentityProvider(ctx context.Context, cluster client.Client, namespace string) error {
	deployments := &appsv1.DeploymentList{}
	if err := cluster.List(ctx, deployments,
		client.InNamespace(namespace),
		client.MatchingLabels{authComponentLabel: "auth"}); err != nil {
		return err
	}
	if len(deployments.Items) == 0 {
		return errors.New("no identity provider Deployment is labelled " + authComponentLabel + "=auth")
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		patch := client.MergeFrom(deployment.DeepCopy())
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = map[string]string{}
		}
		deployment.Spec.Template.Annotations[restartedAtAnnotation] = stamp
		if err := cluster.Patch(ctx, deployment, patch); err != nil {
			return err
		}
	}
	return nil
}
