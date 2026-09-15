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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// The environment the sentences below are about, and the two sentences the
// two paths to Degraded actually write.
const (
	reasonTestEnv = "shop-pr-51"
	// What awaitingDeployTasks records when a deploy task fails: the task,
	// what it did to the deploy, and the run to read.
	taskSentence = "migrate failed before this release could take traffic, so nothing was deployed and " +
		"shop-pr-51 is still serving what it was. Run shop-pr-51-migrate-3: exit code 1; its output is " +
		"in this environment's logs under this run"
	// What updateStatus records when the kubelet refuses a container.
	refusedSentence = "the container of web could not be started: " +
		"CreateContainerConfigError: container has runAsNonRoot and image will run as root"
	// The verdict without the reason: what every failed deploy used to say,
	// and what one with nothing recorded still says.
	deployVerdict = reasonTestEnv + " could not be deployed"
)

// degradedEnvironment is an Environment in the shape one of the two paths
// leaves it in: the sentence on the condition the fault is about, and the same
// sentence on Ready.
func degradedEnvironment(condType, reason, message string) *kitchenv1alpha1.Environment {
	env := &kitchenv1alpha1.Environment{}
	env.Name = reasonTestEnv
	env.Status.Phase = kitchenv1alpha1.EnvironmentDegraded
	for _, t := range []string{condType, condReady} {
		env.Status.Conditions = append(env.Status.Conditions, metav1.Condition{
			Type: t, Status: metav1.ConditionFalse, Reason: reason, Message: message,
			LastTransitionTime: metav1.Now(),
		})
	}
	return env
}

// A failed deployment carries the reason the platform already has (#597).
// Before this every failure of every environment read the same string, which
// is the one fact the reader of a red pull request already had.
func TestAFailedDeploymentSaysWhatTheDeployerKnew(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      *kitchenv1alpha1.Environment
		expected string
	}{{
		name:     "a deploy task that failed",
		env:      degradedEnvironment(condDeployTasks, reasonTaskFailed, taskSentence),
		expected: deployVerdict + ": " + taskSentence,
	}, {
		name:     "a container the kubelet refused",
		env:      degradedEnvironment(condWorkloadAvailable, reasonContainerRefused, refusedSentence),
		expected: deployVerdict + ": " + refusedSentence,
	}, {
		// An environment nobody wrote a sentence onto keeps the old line,
		// which is now the last resort rather than the only answer.
		name:     "an environment that recorded nothing",
		env:      &kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: reasonTestEnv}},
		expected: deployVerdict,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := deploymentDescription(tc.env, gitprovider.DeploymentFailure)
			if got != tc.expected {
				t.Fatalf("the deployment status does not say why:\n got %q\nwant %q", got, tc.expected)
			}
			// The fact leads, because every provider truncates this —
			// GitHub cuts it at 140 characters in gitprovider — so whatever
			// survives the cut still names the environment and the verdict.
			// The property is asserted on the prefix rather than through a
			// truncation of our own: this package's truncate is a plain byte
			// cut and the one that actually runs is the provider's.
			if !strings.HasPrefix(got, deployVerdict) {
				t.Fatalf("the fact does not lead, so truncation would lose it: %q", got)
			}
		})
	}
}

// The other three states are untouched: this issue is about the one that said
// nothing.
func TestTheOtherDeploymentDescriptionsAreUnchanged(t *testing.T) {
	env := &kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: reasonTestEnv}}
	for state, want := range map[gitprovider.DeploymentState]string{
		gitprovider.DeploymentSuccess:    reasonTestEnv + " is live",
		gitprovider.DeploymentInactive:   reasonTestEnv + " was removed",
		gitprovider.DeploymentInProgress: reasonTestEnv + " is deploying",
	} {
		if got := deploymentDescription(env, state); got != want {
			t.Fatalf("%s: got %q, want %q", state, got, want)
		}
	}
}

// The comment is the half of this a reader who cannot sign in gets: the
// description is cut short by the provider and the dashboard link is behind
// the platform's login, so the sentence has to be in the comment body whole.
func TestThePullRequestCommentCarriesTheWholeSentence(t *testing.T) {
	env := degradedEnvironment(condDeployTasks, reasonTaskFailed, taskSentence)
	body := previewComment{
		Environment: reasonTestEnv,
		Phase:       kitchenv1alpha1.EnvironmentDegraded,
		Failure:     previewFailureSentence(env, gitprovider.DeploymentFailure),
	}.body()

	if !strings.Contains(body, "| **Status** | Failed |") {
		t.Fatalf("the comment no longer says the deploy failed:\n%s", body)
	}
	if !strings.Contains(body, "**This deploy did not finish** — "+taskSentence+".\n") {
		t.Fatalf("the comment does not carry the sentence whole:\n%s", body)
	}
	// Nothing here asks the reader to sign in to find out more.
	if strings.Contains(body, "sign in to see") {
		t.Fatalf("the reason is behind the login gate:\n%s", body)
	}
}

// A deploy that is not failing says nothing about a failure, and a preview
// whose reason nobody recorded says that rather than going quiet — "Failed"
// with no account beside it is the whole of what this issue is about.
func TestOnlyAFailedDeployExplainsItself(t *testing.T) {
	env := degradedEnvironment(condDeployTasks, reasonTaskFailed, taskSentence)
	if got := previewFailureSentence(env, gitprovider.DeploymentSuccess); got != "" {
		t.Fatalf("a live environment explains a failure it did not have: %q", got)
	}

	silent := &kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: reasonTestEnv}}
	got := previewFailureSentence(silent, gitprovider.DeploymentFailure)
	if got != "the platform recorded no reason for it" {
		t.Fatalf("a failure with nothing recorded is not admitted to: %q", got)
	}
}

// The condition messages this reads come from six places in the operator and
// they do not agree about the last character.
func TestASentenceEndsExactlyOnce(t *testing.T) {
	for in, want := range map[string]string{
		"nothing ran":  "nothing ran.",
		"nothing ran.": "nothing ran.",
		"  spaced  ":   "spaced.",
		"":             "",
	} {
		if got := fullStop(in); got != want {
			t.Fatalf("fullStop(%q) = %q, want %q", in, got, want)
		}
	}
}
