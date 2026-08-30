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
	"encoding/json"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
)

// The two things a quality gate has to get right, and one it must never do.
//
// Right: a gate that ran and found problems has completed, and a gate that did
// not run has failed. Never: emit a verdict. Everything below is one of those
// three.

const gateArtifactDigest = "sha256:" + "a1b2c3d4e5f6a7b8c9d0" + "a1b2c3d4e5f6a7b8c9d0" +
	"a1b2c3d4e5f6a7b8c9d0" + "abcd"

// gateFixtures is a succeeded build with an artifact, and a platform with one
// gate configured.
func gateFixtures(t *testing.T, gates ...kitchenv1alpha1.QualityGateSpec) (
	*BuildReconciler, *stubAttester, *kitchenv1alpha1.Build,
) {
	t.Helper()
	if len(gates) == 0 {
		gates = []kitchenv1alpha1.QualityGateSpec{{
			Name:    "trivy",
			Image:   "aquasec/trivy:0.58.0",
			Version: "0.58.0",
			Format:  "trivy-json",
			Args:    []string{"image", "--format", "json", "--output", "$(KITCHEN_FINDINGS)", "$(KITCHEN_ARTIFACT)"},
		}}
	}
	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			Compliance: kitchenv1alpha1.ComplianceSpec{
				Attestation: kitchenv1alpha1.AttestationSpec{Enabled: true},
				Gates:       gates,
			},
		},
	}
	connection := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "dockerRegistry",
			CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "registry-creds"},
			Config:               &runtime.RawExtension{Raw: []byte(`{"url":"registry.example.com/kitchen"}`)},
		},
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: PlatformNamespace},
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(
				`{"auths":{"registry.example.com":{"username":"robot","password":"hunter2"}}}`),
		},
	}
	_, privatePEM, publicPEM, err := attestation.GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SigningKeySecretName, Namespace: PlatformNamespace},
		Data: map[string][]byte{
			attestation.SecretKeyPrivate: privatePEM,
			attestation.SecretKeyPublic:  publicPEM,
		},
	}
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source: kitchenv1alpha1.GitSourceSpec{Repo: "acme/shop"},
			Registry: kitchenv1alpha1.RegistrySpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
			},
		},
	}
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-1", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Git:        kitchenv1alpha1.GitRevision{SHA: "abc123def456", Branch: "main"},
		},
		Status: kitchenv1alpha1.BuildStatus{
			Phase: kitchenv1alpha1.BuildSucceeded,
			Artifact: &kitchenv1alpha1.ArtifactStatus{
				Repository: "registry.example.com/kitchen/shop",
				Digest:     gateArtifactDigest,
			},
		},
	}

	attester := &stubAttester{blobs: map[string][]byte{}}
	reconciler := &BuildReconciler{
		Client:           complianceClient(t, kitchen, connection, creds, keySecret, project, build),
		QualityGateImage: "ghcr.io/bermos/kitchen:0.9.0",
		Attesters: func([]byte, string) (ArtifactAttester, error) {
			return attester, nil
		},
	}
	return reconciler, attester, build
}

func TestGatesRunAsAJobThatIsGivenTheArtifactAndNothingElse(t *testing.T) {
	reconciler, _, build := gateFixtures(t)

	if _, err := reconciler.reconcileGates(context.Background(), build); err != nil {
		t.Fatal(err)
	}

	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: appNamespace("shop"), Name: "shop-bld-1-gate-trivy"}
	if err := reconciler.Get(context.Background(), key, job); err != nil {
		t.Fatalf("no gate Job was created: %v", err)
	}
	pod := job.Spec.Template.Spec

	// The gate runs first and is an image somebody else wrote. It gets the
	// artifact, a credential to pull it, and nothing that would let it touch
	// the cluster.
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Image != "aquasec/trivy:0.58.0" {
		t.Fatalf("the gate is not the init container: %+v", pod.InitContainers)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the gate pod was given a service account token")
	}
	if security := pod.InitContainers[0].SecurityContext; security == nil ||
		security.RunAsNonRoot == nil || !*security.RunAsNonRoot {
		t.Error("the gate runs as root")
	}

	// Kubernetes' own $(VAR) expansion is what points a gate at the artifact,
	// so a gate is configured with the environment rather than with templating
	// of Kitchen's.
	environment := map[string]string{}
	for _, variable := range pod.InitContainers[0].Env {
		environment[variable.Name] = variable.Value
	}
	if want := "registry.example.com/kitchen/shop@" + gateArtifactDigest; environment["KITCHEN_ARTIFACT"] != want {
		t.Errorf("the gate was pointed at %q, want %q", environment["KITCHEN_ARTIFACT"], want)
	}
	if environment["KITCHEN_FINDINGS"] == "" || environment["DOCKER_CONFIG"] == "" {
		t.Errorf("the gate was not told where to write or what to pull with: %+v", environment)
	}

	// The publisher runs second, in the operator's own image, and is the only
	// thing that carries findings out.
	if len(pod.Containers) != 1 || pod.Containers[0].Image != "ghcr.io/bermos/kitchen:0.9.0" {
		t.Fatalf("the publisher is not the container: %+v", pod.Containers)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("a gate that failed to run will be retried, which would leave the build's evidence " +
			"disagreeing with its history")
	}
	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Error("a gate can hang forever")
	}

	if len(build.Status.Gates) != 1 || build.Status.Gates[0].Phase != kitchenv1alpha1.GateRunning {
		t.Errorf("the build does not say the gate is running: %+v", build.Status.Gates)
	}
}

func TestAGateThatRanIsCompletedAndItsFindingsAreSigned(t *testing.T) {
	reconciler, attester, build := gateFixtures(t)
	ctx := context.Background()

	if _, err := reconciler.reconcileGates(ctx, build); err != nil {
		t.Fatal(err)
	}
	// Findings a scanner would call bad news. The gate completed all the same:
	// it did exactly its job.
	findings := []byte(`{"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-2026-1","Severity":"CRITICAL"}]}]}`)
	blob := "sha256:" + strings.Repeat("b", 64)
	attester.blobs[blob] = findings
	finishGateJob(t, reconciler, build, "trivy", gateReport{Gate: "trivy", Blob: blob, Bytes: len(findings)})

	if _, err := reconciler.reconcileGates(ctx, build); err != nil {
		t.Fatal(err)
	}

	if len(build.Status.Gates) != 1 {
		t.Fatalf("the build records %d gates, want 1", len(build.Status.Gates))
	}
	gate := build.Status.Gates[0]
	if gate.Phase != kitchenv1alpha1.GateCompleted {
		t.Errorf("a gate that ran and found a critical vulnerability is %q, want Completed — "+
			"finding problems is the gate working", gate.Phase)
	}
	if gate.Message != "" {
		t.Errorf("the gate reports a problem of its own: %q", gate.Message)
	}
	if gate.Attested == nil {
		t.Error("the findings were not signed")
	}
	if gate.Source != "platform" {
		t.Errorf("the result is credited to %q, want the platform", gate.Source)
	}

	if len(attester.attached) != 1 {
		t.Fatalf("attached %d envelopes, want the gate's findings", len(attester.attached))
	}
	statement, err := attester.attached[0].Statement()
	if err != nil {
		t.Fatal(err)
	}
	if statement.PredicateType != attestation.PredicateQualityGate {
		t.Errorf("the findings were attached as %s", statement.PredicateType)
	}
	if !statement.Describes(gateArtifactDigest) {
		t.Error("the findings are not about the artifact")
	}

	predicate := map[string]any{}
	if err := json.Unmarshal(statement.Predicate, &predicate); err != nil {
		t.Fatal(err)
	}
	// The rule the whole design turns on. A gate records what it found; what
	// is disqualifying is a property of the environment being deployed to, and
	// answering it here would fix it platform-wide at the moment of scanning.
	for _, forbidden := range []string{"pass", "passed", "verdict", "result", "ok", "allowed"} {
		if _, present := predicate[forbidden]; present {
			t.Errorf("the quality gate predicate carries a verdict field %q", forbidden)
		}
	}
	if predicate["gate"] != "trivy" || predicate["version"] != "0.58.0" {
		t.Errorf("the gate did not identify itself: %+v", predicate)
	}
	// The findings are carried as the bytes the gate wrote, not re-shaped.
	carried, err := json.Marshal(predicate["findings"])
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(carried) || !strings.Contains(string(carried), "CVE-2026-1") {
		t.Errorf("the findings did not survive: %s", carried)
	}
}

func TestAGateThatDidNotRunIsFailedAndSaysSo(t *testing.T) {
	reconciler, attester, build := gateFixtures(t)
	ctx := context.Background()

	if _, err := reconciler.reconcileGates(ctx, build); err != nil {
		t.Fatal(err)
	}
	failGateJob(t, reconciler, build, "trivy")

	if _, err := reconciler.reconcileGates(ctx, build); err != nil {
		t.Fatal(err)
	}

	gate := build.Status.Gates[0]
	if gate.Phase != kitchenv1alpha1.GateFailed {
		t.Errorf("a gate that never ran is %q, want Failed", gate.Phase)
	}
	if gate.Message == "" {
		t.Error("a gate that did not run says nothing about why")
	}
	if gate.Attested != nil {
		t.Error("a gate that did not run produced signed evidence")
	}
	if len(attester.attached) != 0 {
		t.Error("something was attached for a gate that never ran")
	}
}

func TestAGateWhoseFindingsWentMissingIsNotReportedAsClean(t *testing.T) {
	// The publisher completed but its report names a blob the registry does
	// not have. The gate ran, so it completed — but nothing was signed, and
	// the status has to say that rather than look like a clean scan.
	reconciler, attester, build := gateFixtures(t)
	ctx := context.Background()

	if _, err := reconciler.reconcileGates(ctx, build); err != nil {
		t.Fatal(err)
	}
	finishGateJob(t, reconciler, build, "trivy",
		gateReport{Gate: "trivy", Blob: "sha256:" + strings.Repeat("c", 64), Bytes: 10})

	if _, err := reconciler.reconcileGates(ctx, build); err != nil {
		t.Fatal(err)
	}

	gate := build.Status.Gates[0]
	if gate.Attested != nil {
		t.Error("findings that could not be read were recorded as attested")
	}
	if gate.Message == "" {
		t.Error("findings went missing without the status saying so")
	}
	if len(attester.attached) != 0 {
		t.Error("an envelope was attached over findings that could not be read")
	}
}

func TestADisabledGateDoesNotRun(t *testing.T) {
	reconciler, _, build := gateFixtures(t, kitchenv1alpha1.QualityGateSpec{
		Name: "trivy", Image: "aquasec/trivy:0.58.0", Disabled: true,
	})

	if _, err := reconciler.reconcileGates(context.Background(), build); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := reconciler.List(context.Background(), jobs, client.InNamespace(appNamespace("shop"))); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("a disabled gate ran anyway: %d jobs", len(jobs.Items))
	}
	if len(build.Status.Gates) != 0 {
		t.Errorf("a disabled gate was recorded: %+v", build.Status.Gates)
	}
}

func TestGateJobNameStaysWithinWhatKubernetesAllows(t *testing.T) {
	long := strings.Repeat("g", 40)
	name := gateJobName("shop-bld-abcdef-xk2p9", long)
	if len(name) > 63 {
		t.Errorf("the job name is %d characters: %s", len(name), name)
	}
	// The build name is what makes the name unique, so it is the gate name
	// that gives way — truncating the build would collide across builds.
	if !strings.HasPrefix(name, "shop-bld-abcdef-xk2p9-gate-") {
		t.Errorf("the build name did not survive: %s", name)
	}
}

// finishGateJob marks a gate's Job complete and leaves the publisher's report
// on a pod, which is how the operator learns where the findings went.
func finishGateJob(t *testing.T, r *BuildReconciler, build *kitchenv1alpha1.Build, gate string, report gateReport) {
	t.Helper()
	ctx := context.Background()
	name := gateJobName(build.Name, gate)
	appNS := appNamespace("shop")

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: appNS, Name: name}, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	}}
	if err := r.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-pod", Namespace: appNS, Labels: map[string]string{"job-name": name},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "publish",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: string(body),
			}},
		}}},
	}
	if err := r.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := r.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

// failGateJob marks a gate's Job failed, which is what a scanner that crashed
// or an image that would not pull leaves behind.
func failGateJob(t *testing.T, r *BuildReconciler, build *kitchenv1alpha1.Build, gate string) {
	t.Helper()
	ctx := context.Background()
	job := &batchv1.Job{}
	key := types.NamespacedName{Namespace: appNamespace("shop"), Name: gateJobName(build.Name, gate)}
	if err := r.Get(ctx, key, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "the scanner exited 137",
	}}
	if err := r.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
}
