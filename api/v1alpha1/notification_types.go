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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Outbound notifications, in two objects.
//
// A build that fails at 02:00 is found by whoever opens the dashboard next.
// Everything needed to say so out loud is already recorded — the reconcilers
// write every build, release and environment transition into the activity feed
// — and nothing pushed it anywhere. These are the two objects that push it.
//
// # Why two objects rather than one
//
// A NotificationSubscription is configuration: where to send, which events,
// whose events. It changes when a person changes it.
//
// A NotificationDelivery is one event on its way to one subscription, and it
// is a separate object for three reasons, each of which is a requirement
// rather than a preference:
//
//   - **A failing notification must never affect what it reports on.** The
//     thing that decides to notify is on a reconcile path — the Build
//     controller, the Environment controller — and the thing that talks to
//     somebody else's HTTP server must not be. Creating a Delivery is the
//     whole of what happens on that path, and even that is best-effort: it is
//     done off the reconcile's own goroutine and a failure to create one is a
//     log line (see internal/notify).
//   - **At-least-once has to survive a restart.** A queue in the operator's
//     memory loses everything in flight when the pod moves, which is exactly
//     the moment a platform is most worth hearing from. A Delivery is in etcd
//     before the first attempt is made, so the retry ladder is picked up by
//     whichever replica holds the lease next.
//   - **The dead letter has to be visible.** "It was never delivered" is a
//     thing a person has to be able to see, and a bounded ring buffer inside
//     the subscription's status would either be too small to be useful or too
//     large to belong in a status. A dead letter is a Delivery whose phase says
//     so; the dashboard lists them under the subscription, and `kitchen api GET
//     /notifications/deliveries?phase=DeadLettered` prints them.
//
// Deliveries are owned by their subscription, so deleting one takes its whole
// history with it, and the reconciler prunes what it has finished with (see
// DeliveryRetention below).

// NotificationEvent is one of the things a subscription can ask to hear about.
//
// The vocabulary is deliberately the *platform's* rather than the
// reconcilers': a subscriber is a relay somebody wrote in an afternoon, and it
// should not have to know that a promotion, an auto-deploy and a rollback are
// three code paths that all mean a release went live.
//
// +kubebuilder:validation:Enum=build.failed;deploy.succeeded;preview.created;preview.destroyed;environment.unhealthy;alert.firing
type NotificationEvent string

const (
	// NotifyBuildFailed is a build that ended in failure. The commit is the
	// one thing every relay wants and it is in the payload.
	NotifyBuildFailed NotificationEvent = "build.failed"
	// NotifyDeploySucceeded is a release going live on an environment — by
	// promotion, by auto-deploy on a push, or by rollback. All three are one
	// event because all three are one fact: what is serving changed, and it
	// is now this.
	NotifyDeploySucceeded NotificationEvent = "deploy.succeeded"
	// NotifyPreviewCreated is a preview environment being published for a
	// pull request.
	NotifyPreviewCreated NotificationEvent = "preview.created"
	// NotifyPreviewDestroyed is that preview going away again.
	NotifyPreviewDestroyed NotificationEvent = "preview.destroyed"
	// NotifyEnvironmentUnhealthy is an environment that stopped being
	// healthy without anybody deploying anything — the case the pull request
	// a deploy came from is no longer watching.
	NotifyEnvironmentUnhealthy NotificationEvent = "environment.unhealthy"
	// NotifyAlertFiring is a saved log query that crossed its threshold. It
	// is the second trigger onto this same delivery path, and the reason the
	// path is generic (see SavedQueryAlert).
	NotifyAlertFiring NotificationEvent = "alert.firing"
)

// AllNotificationEvents is the vocabulary, in the order the dashboard offers
// it. It is one list so that the API's validation, the dashboard's checkboxes
// and the documentation cannot disagree about what may be subscribed to.
var AllNotificationEvents = []NotificationEvent{
	NotifyDeploySucceeded,
	NotifyBuildFailed,
	NotifyEnvironmentUnhealthy,
	NotifyPreviewCreated,
	NotifyPreviewDestroyed,
	NotifyAlertFiring,
}

// NotificationSubscriptionSpec is where to send what.
type NotificationSubscriptionSpec struct {
	// URL is the endpoint every matching event is POSTed to. It must be
	// absolute and `https` — a signed payload over plain HTTP is a payload
	// anybody on the path can read, and the signature only proves it was not
	// changed.
	//
	// A relay is what a chat vendor's integration is here: the platform ships
	// one shape of payload, signed, and Slack, Discord and Teams are each
	// forty lines of somebody else's code in front of it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2000
	URL string `json:"url"`

	// Events is what this subscription wants to hear about. An empty list is
	// refused at admission rather than treated as "everything": a
	// subscription that silently widens when the platform learns a new event
	// type is a subscription that starts paging somebody at 03:00 because of
	// an upgrade.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Events []NotificationEvent `json:"events"`

	// ProjectRef narrows the subscription to one project. Empty is the
	// platform scope: every project's events, which is the operator's
	// subscription and nobody else's — the API refuses to write one for
	// anybody but an operator.
	// +optional
	ProjectRef *LocalObjectReference `json:"projectRef,omitempty"`

	// SecretRef names the Secret holding the signing key, under the key
	// `secret`. The API writes it from the create request and never reads it
	// back out; rotating it is another write of a value that also never
	// comes back.
	//
	// The Secret is owned by this object, so deleting the subscription
	// deletes the key with it.
	SecretRef LocalObjectReference `json:"secretRef"`

	// Description is what this subscription is for, in the words of whoever
	// made it. A URL alone stops meaning anything about four months after it
	// is written down.
	// +kubebuilder:validation:MaxLength=500
	// +optional
	Description string `json:"description,omitempty"`

	// Suspended stops delivery without deleting anything. It is the switch
	// for a receiver that has gone down and is filling the dead letter with
	// the same failure: nothing new is queued while it is set, and what is
	// already queued waits.
	// +optional
	Suspended bool `json:"suspended,omitempty"`

	// MaxAttempts bounds the retry ladder. The attempt that exhausts it is
	// the one that dead-letters the delivery.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +optional
	MaxAttempts int32 `json:"maxAttempts,omitempty"`

	// TimeoutSeconds bounds one attempt. A receiver that takes longer than
	// this has not accepted the event, and the attempt counts as a failure.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=30
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// CreatedBy is who made it, as the API knew them. Recorded rather than
	// enforced: a subscription belongs to the project or to the platform,
	// not to a person, and this is a byline.
	// +optional
	CreatedBy string `json:"createdBy,omitempty"`
}

// DefaultMaxAttempts and DefaultTimeoutSeconds are what a subscription that
// says nothing gets. Five attempts on the ladder in notify.Backoff span two and
// a half minutes, which covers a receiver being restarted and stops well short
// of one that is gone.
const (
	DefaultMaxAttempts    int32 = 5
	DefaultTimeoutSeconds int32 = 10
)

// Attempts is the ladder length this subscription asked for.
func (s NotificationSubscriptionSpec) Attempts() int32 {
	if s.MaxAttempts <= 0 {
		return DefaultMaxAttempts
	}
	return s.MaxAttempts
}

// Timeout is how long one attempt may take, in seconds.
func (s NotificationSubscriptionSpec) Timeout() int32 {
	if s.TimeoutSeconds <= 0 {
		return DefaultTimeoutSeconds
	}
	return s.TimeoutSeconds
}

// Project is the project this subscription is scoped to, empty for the
// platform scope.
func (s NotificationSubscriptionSpec) Project() string {
	if s.ProjectRef == nil {
		return ""
	}
	return s.ProjectRef.Name
}

// Wants reports whether this subscription asked to hear about an event.
func (s NotificationSubscriptionSpec) Wants(event NotificationEvent) bool {
	for _, selected := range s.Events {
		if selected == event {
			return true
		}
	}
	return false
}

// NotificationSubscriptionStatus is what has actually happened to it.
type NotificationSubscriptionStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Delivered, Failed and DeadLettered count attempts and outcomes since
	// the subscription was created. They are counters rather than a list
	// because the list is the Deliveries themselves, and a status that grew
	// with traffic would be a status nobody could read.
	// +optional
	Delivered int64 `json:"delivered,omitempty"`
	// +optional
	Failed int64 `json:"failed,omitempty"`
	// +optional
	DeadLettered int64 `json:"deadLettered,omitempty"`

	// LastDeliveryTime and LastResult are the most recent outcome, which is
	// the one thing a person checking "is this working" is looking for.
	// +optional
	LastDeliveryTime *metav1.Time `json:"lastDeliveryTime,omitempty"`
	// +kubebuilder:validation:Enum=delivered;failed
	// +optional
	LastResult string `json:"lastResult,omitempty"`
	// +optional
	LastStatusCode int32 `json:"lastStatusCode,omitempty"`
	// LastError is why the last attempt failed, empty after one that did not.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="Last",type=string,JSONPath=`.status.lastResult`
// +kubebuilder:printcolumn:name="Dead",type=integer,JSONPath=`.status.deadLettered`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NotificationSubscription is the Schema for the notificationsubscriptions API.
type NotificationSubscription struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NotificationSubscriptionSpec   `json:"spec,omitempty"`
	Status NotificationSubscriptionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NotificationSubscriptionList contains a list of NotificationSubscription.
type NotificationSubscriptionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NotificationSubscription `json:"items"`
}

// NotificationDeliveryPhase is where one delivery has got to.
//
// +kubebuilder:validation:Enum=Pending;Delivered;DeadLettered
type NotificationDeliveryPhase string

const (
	// DeliveryPending is queued, or waiting out a backoff between attempts.
	DeliveryPending NotificationDeliveryPhase = "Pending"
	// DeliveryDelivered is accepted by the receiver: one 2xx, once.
	DeliveryDelivered NotificationDeliveryPhase = "Delivered"
	// DeliveryDeadLettered is the bound being reached. Nothing retries it on
	// its own after this; a person can, from the dashboard or the API, and
	// the payload it holds is exactly the one that would have been sent.
	DeliveryDeadLettered NotificationDeliveryPhase = "DeadLettered"
)

// NotificationDeliverySpec is one event on its way to one subscription.
//
// The payload is stored as the exact bytes that will be sent, not as fields to
// be re-marshalled per attempt. That is what makes the signature reproducible:
// a receiver verifies an HMAC over the body it received, and a body whose key
// order changed between attempt one and attempt four would verify on one and
// not the other.
type NotificationDeliverySpec struct {
	// SubscriptionRef is the subscription this is for. The object is also
	// this delivery's owner, so the reference cannot dangle for long.
	SubscriptionRef LocalObjectReference `json:"subscriptionRef"`

	// Event is what happened, and EventID identifies it. The id is the
	// receiver's idempotency key: every attempt of this delivery carries the
	// same one, and at-least-once means a receiver will see a repeat.
	Event NotificationEvent `json:"event"`
	// +kubebuilder:validation:MinLength=1
	EventID string `json:"eventId"`

	// Payload is the JSON body, verbatim.
	// +kubebuilder:validation:MaxLength=16384
	Payload string `json:"payload"`

	// Project is what the event was about, empty for a platform event. It is
	// duplicated out of the payload because it is what the API filters a
	// caller's view of the deliveries by, and parsing every payload to
	// answer one list request is not a filter.
	// +optional
	Project string `json:"project,omitempty"`
}

// NotificationDeliveryAttempt is one attempt, kept so that "it failed" can be
// read as "it failed like this, four times, over nine minutes".
type NotificationDeliveryAttempt struct {
	Number int32       `json:"number"`
	Time   metav1.Time `json:"time"`
	// StatusCode is the receiver's, or 0 when there was no response at all.
	// +optional
	StatusCode int32 `json:"statusCode,omitempty"`
	// Error is why it failed, empty on the one that did not. It is the
	// transport's message or the receiver's status line — never the
	// receiver's body, which is somebody else's data and may be anything.
	// +optional
	Error string `json:"error,omitempty"`
	// DurationMillis is how long the attempt took.
	// +optional
	DurationMillis int64 `json:"durationMillis,omitempty"`
}

// NotificationDeliveryStatus is how it is getting on.
type NotificationDeliveryStatus struct {
	// +optional
	Phase NotificationDeliveryPhase `json:"phase,omitempty"`

	// Attempts is how many have been made, and Attempted the last few of
	// them. The list is bounded (maxDeliveryAttemptRecords) because the
	// ladder is bounded; it is the whole ladder in practice.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Attempted []NotificationDeliveryAttempt `json:"attempted,omitempty"`

	// NextAttemptTime is when the next attempt is due. It is the backoff,
	// made explicit: the reconciler requeues to it rather than sleeping, so a
	// restart resumes the ladder instead of restarting it.
	// +optional
	NextAttemptTime *metav1.Time `json:"nextAttemptTime,omitempty"`

	// CompletedTime is when it was delivered or dead-lettered, and what the
	// pruner ages a finished delivery from.
	// +optional
	CompletedTime *metav1.Time `json:"completedTime,omitempty"`

	// LastError and LastStatusCode are the most recent attempt's, lifted out
	// of the list so that a printer column and a list row can show why
	// without reading it.
	// +optional
	LastError string `json:"lastError,omitempty"`
	// +optional
	LastStatusCode int32 `json:"lastStatusCode,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Event",type=string,JSONPath=`.spec.event`
// +kubebuilder:printcolumn:name="Subscription",type=string,JSONPath=`.spec.subscriptionRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Attempts",type=integer,JSONPath=`.status.attempts`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NotificationDelivery is the Schema for the notificationdeliveries API.
type NotificationDelivery struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NotificationDeliverySpec   `json:"spec,omitempty"`
	Status NotificationDeliveryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NotificationDeliveryList contains a list of NotificationDelivery.
type NotificationDeliveryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NotificationDelivery `json:"items"`
}

// Done reports whether a delivery has stopped moving on its own.
func (d *NotificationDelivery) Done() bool {
	return d.Status.Phase == DeliveryDelivered || d.Status.Phase == DeliveryDeadLettered
}

func init() {
	SchemeBuilder.Register(
		&NotificationSubscription{}, &NotificationSubscriptionList{},
		&NotificationDelivery{}, &NotificationDeliveryList{},
	)
}
