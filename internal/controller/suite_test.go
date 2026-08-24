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
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	// Gomega's default budget is one second, which is the wrong order of
	// magnitude for anything driven against a real API server. A teardown
	// that waits on the project finalizer is the case that proves it: it
	// needs two reconcile passes, each listing seven kinds, and the *first*
	// one in a process also pays for the client's discovery and REST mapping
	// of those kinds. Measured, that first teardown takes about 1.2 seconds
	// where every later one takes 0.2 — so the default fails the first spec
	// to try it and passes the rest, which reads as a flake and is not one.
	//
	// The budget is a ceiling, not a wait: Eventually returns the moment its
	// condition holds, so a generous one costs nothing when things are well
	// and reports honestly when they are not.
	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(50 * time.Millisecond)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = kitchenv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = gatewayv1.Install(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = gatewayv1beta1.Install(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "test", "crd", "gateway-api"),
			// cert-manager's own schemas, extracted from the sub-chart the
			// platform installs by hack/gen-test-crds.sh. The operator writes
			// its ClusterIssuer and Certificate as unstructured objects, so
			// only a real CRD prunes a misspelled field — which is what turns
			// a typo in those specs into a failing assertion here rather than
			// a passing round-trip.
			filepath.Join("..", "..", "test", "crd", "cert-manager"),
			// The KEDA HTTP add-on's HTTPScaledObject, for the same reason.
			// It comes from a chart the platform does *not* install, so it is
			// fetched from the add-on release rather than from charts/.
			filepath.Join("..", "..", "test", "crd", "keda-http"),
		},
		ErrorIfCRDPathMissing: true,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// acmeTLS is the TLS block every Kitchen created through the API server needs.
// acme is the default mode, and the CRD refuses that mode without an ACME
// account and a solver, so a spec that only cares about the base domain still
// has to say how the platform would get its certificate.
func acmeTLS() kitchenv1alpha1.TLSSpec {
	return kitchenv1alpha1.TLSSpec{
		Mode: kitchenv1alpha1.TLSModeACME,
		ACME: &kitchenv1alpha1.ACMESpec{
			Email: "platform@example.com",
			DNS01: kitchenv1alpha1.ACMEDNS01Spec{
				Cloudflare: &kitchenv1alpha1.CloudflareSolverSpec{
					APITokenSecretRef: kitchenv1alpha1.SecretKeySelector{
						Name: "cloudflare-api-token",
						Key:  "api-token",
					},
				},
			},
		},
	}
}

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
