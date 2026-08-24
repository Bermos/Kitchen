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

// The controller names an audit record is attributed to when the platform
// decided the transition itself. They are the reconciler's own names, so that
// "who did this" answers with something an operator can go and read.
const (
	// actorAccessReviewController attributes what the recertification
	// machinery does on its own: opening a cycle on the cadence, stamping
	// one overdue, applying the revocations a closed cycle decided, and
	// reporting a write to a Kitchen object that no reconcile made.
	actorAccessReviewController   = "accessreview"
	actorBuildController          = "build"
	actorConnectionController     = "connection"
	actorDomainController         = "domain"
	actorEnvironmentController    = "environment"
	actorExceptionController      = "exception"
	actorPlatformUpdateController = "platformupdate"
	// actorPolicyEngine attributes policy decisions the platform evaluated on
	// its own initiative — a promotion applied automatically, a scheduled
	// rescan. A decision a person asked for (a replay through the API)
	// carries the caller instead, like every API write.
	actorPolicyEngine            = "policy"
	actorProjectController       = "project"
	actorPromotionController     = "promotion"
	actorReleaseController       = "release"
	actorResourceClaimController = "resourceclaim"
	// actorRescanSweep attributes what the continuous re-evaluation pass does
	// on its own initiative — today, the rollback an expired exception asked
	// for. The decisions it produces are the policy engine's and carry
	// actorPolicyEngine, because a verdict is the engine's word wherever it
	// was asked for.
	actorRescanSweep = "rescan"
)
