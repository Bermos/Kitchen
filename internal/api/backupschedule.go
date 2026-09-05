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

package api

import (
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/backup"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The scheduled backup's own surface: where archives go, what is there, and
// running one now.
//
// The split between these routes and `PATCH /settings` is deliberate and is
// the whole reason the destination has an address of its own: **it carries a
// credential, and the settings route must never carry one.** Schedule,
// suspend and retention are ordinary settings and go through the route that
// already edits this object; the bucket's access key does not, because a
// credential write is a different kind of request — it creates a Secret, it
// is never echoed back, and it is worth being able to find in an audit log on
// its own.
//
// `POST /platform/backup/runs` is what makes a destination testable: press it
// once on the day it is configured and find out whether the credential works,
// rather than at 02:00 six weeks later.

const (
	// BackupDestinationSecretName holds the destination's access key pair. It
	// carries the managed-by label every credential this API writes carries,
	// so removing the destination removes the platform's own Secret and never
	// one something else put there.
	BackupDestinationSecretName = "kitchen-backup-destination"

	// backupRunTTLSeconds collects a run started by hand. The CronJob's own
	// history limits collect the runs it owns; this copy is owned by nobody.
	backupRunTTLSeconds = 24 * 60 * 60

	// maxBackupObjects bounds a listing of the destination. A bucket that has
	// been taking a nightly archive for years is a long list, and the screen
	// reads the newest.
	maxBackupObjects = 200
)

// backupScheduleView is the scheduled backup as the dashboard reads it: what
// is configured, and what it has actually been doing.
type backupScheduleView struct {
	// Schedule is the five-field cron expression, in UTC. Empty is an
	// installation with no scheduled backup, which is the state this whole
	// surface exists to get an installation out of.
	Schedule string `json:"schedule,omitempty"`
	// Suspended is a schedule deliberately paused.
	Suspended bool `json:"suspended"`
	// TimeoutMinutes bounds one run.
	TimeoutMinutes int32 `json:"timeoutMinutes"`

	// Destination is where archives go, described and never echoed.
	Destination *backupDestinationView `json:"destination,omitempty"`

	// KeepLast and KeepDays are the retention. Absent is "keep everything",
	// which is the safe default.
	KeepLast *int32 `json:"keepLast,omitempty"`
	KeepDays *int32 `json:"keepDays,omitempty"`

	// LastRun, LastSuccess and LastFailure are the numbers worth watching.
	// LastSuccess is the one that matters: a platform whose last successful
	// backup was in March should say so on its own status, unasked.
	LastRun            *time.Time `json:"lastRun,omitempty"`
	LastSuccess        *time.Time `json:"lastSuccess,omitempty"`
	LastSuccessArchive string     `json:"lastSuccessArchive,omitempty"`
	LastSuccessBytes   int64      `json:"lastSuccessBytes,omitempty"`
	LastFailure        *time.Time `json:"lastFailure,omitempty"`
	Archives           int32      `json:"archives,omitempty"`

	// Ready, Reason and Message are the BackupReady condition as the operator
	// wrote it. They are served rather than re-derived here so that the
	// screen and the platform's own status cannot come to disagree.
	Ready   bool   `json:"ready"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// backupDestinationView describes a destination and never its credential.
type backupDestinationView struct {
	Type string `json:"type"`
	// Described is the destination as a person reads it: "s3://bucket/prefix".
	Described            string `json:"described"`
	Bucket               string `json:"bucket,omitempty"`
	Prefix               string `json:"prefix,omitempty"`
	Region               string `json:"region,omitempty"`
	Endpoint             string `json:"endpoint,omitempty"`
	ForcePathStyle       bool   `json:"forcePathStyle,omitempty"`
	ServerSideEncryption string `json:"serverSideEncryption,omitempty"`
	KMSKeyID             string `json:"kmsKeyId,omitempty"`
	// Credential says *how* the destination authenticates and never what
	// with: a key this platform stores, or the ambient chain.
	Credential string `json:"credential"`
}

// The two things `credential` can say. Neither is a key.
const (
	backupCredentialStored  = "stored"
	backupCredentialAmbient = "ambient"
)

// newBackupScheduleView reads the singleton into the answer.
func newBackupScheduleView(kitchen *kitchenv1alpha1.Kitchen) backupScheduleView {
	spec := kitchen.Spec.Backup
	view := backupScheduleView{
		Schedule:       spec.Schedule,
		Suspended:      spec.Suspend,
		TimeoutMinutes: int32(controller.BackupTimeoutOf(kitchen).Minutes()),
		KeepLast:       spec.Retention.KeepLast,
		KeepDays:       spec.Retention.KeepDays,
	}
	if destination := spec.Destination; destination != nil {
		described := &backupDestinationView{
			Type:       string(destination.Type),
			Described:  backup.Describe(destination),
			Credential: backupCredentialAmbient,
		}
		if s3 := destination.S3; s3 != nil {
			described.Bucket = s3.Bucket
			described.Prefix = s3.Prefix
			described.Region = s3.Region
			described.Endpoint = s3.Endpoint
			described.ForcePathStyle = s3.ForcePathStyle
			described.ServerSideEncryption = s3.ServerSideEncryption
			described.KMSKeyID = s3.KMSKeyID
			if s3.CredentialsSecretRef != nil {
				described.Credential = backupCredentialStored
			}
		}
		view.Destination = described
	}
	if status := kitchen.Status.Backup; status != nil {
		view.LastRun = timeOrNil(status.LastRun)
		view.LastSuccess = timeOrNil(status.LastSuccess)
		view.LastSuccessArchive = status.LastSuccessArchive
		view.LastSuccessBytes = status.LastSuccessBytes
		view.LastFailure = timeOrNil(status.LastFailure)
		view.Archives = status.Archives
		view.Message = status.Message
	}
	if condition := meta.FindStatusCondition(kitchen.Status.Conditions, controller.ConditionBackupReady); condition != nil {
		view.Ready = condition.Status == metav1.ConditionTrue
		view.Reason = condition.Reason
		view.Message = condition.Message
	}
	return view
}

// timeOrNil unwraps a status timestamp for JSON.
func timeOrNil(at *metav1.Time) *time.Time {
	if at == nil {
		return nil
	}
	when := at.Time
	return &when
}

// putBackupDestinationRequest is where archives are to go, and the credential
// to write them with. The credential half is write-only: nothing in this
// package ever serializes it back out.
type putBackupDestinationRequest struct {
	// Type is the destination kind. Empty means s3, which is the only one.
	Type string              `json:"type,omitempty"`
	S3   *s3DestinationWrite `json:"s3,omitempty"`
}

type s3DestinationWrite struct {
	Bucket               string `json:"bucket"`
	Prefix               string `json:"prefix,omitempty"`
	Region               string `json:"region,omitempty"`
	Endpoint             string `json:"endpoint,omitempty"`
	ForcePathStyle       bool   `json:"forcePathStyle,omitempty"`
	ServerSideEncryption string `json:"serverSideEncryption,omitempty"`
	KMSKeyID             string `json:"kmsKeyId,omitempty"`

	// AccessKeyID and SecretAccessKey are the credential. Both or neither:
	// half a key pair is a destination that cannot authenticate, and it
	// should be refused here rather than discovered on the first run.
	AccessKeyID     string `json:"accessKeyId,omitempty"`
	SecretAccessKey string `json:"secretAccessKey,omitempty"`

	// AmbientCredentials moves this destination onto the credential chain the
	// pod already has — IRSA, EKS Pod Identity, an instance role — and
	// deletes the key this platform was storing. It is explicit because
	// omitting the keys means "leave the credential alone": the API never
	// reads a credential back, so a form that redisplays a destination cannot
	// send the key it never received, and an unmentioned key must survive the
	// edit.
	AmbientCredentials bool `json:"ambientCredentials,omitempty"`
}

// putBackupDestination writes where archives go.
//
// The Secret is the operator's, carries the managed-by label, and is never
// read back — the same rule every credential this API stores follows. The
// response echoes the bucket and the prefix and no key.
func (s *Server) putBackupDestination(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := putBackupDestinationRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	kind := strings.TrimSpace(body.Type)
	if kind == "" {
		kind = string(kitchenv1alpha1.BackupDestinationS3)
	}
	if kind != string(kitchenv1alpha1.BackupDestinationS3) {
		badRequest(w, "type must be %q (got %q); a second destination kind is a new value in the enum "+
			"and an implementation behind it", kitchenv1alpha1.BackupDestinationS3, body.Type)
		return
	}
	if body.S3 == nil {
		badRequest(w, "an s3 destination needs an s3 block naming the bucket")
		return
	}
	if strings.TrimSpace(body.S3.Bucket) == "" {
		badRequest(w, "bucket is required: it is where archives are written")
		return
	}
	switch encryption := body.S3.ServerSideEncryption; encryption {
	case "", kitchenv1alpha1.ServerSideEncryptionAES256, kitchenv1alpha1.ServerSideEncryptionKMS:
	default:
		badRequest(w, "serverSideEncryption must be %s or %s (got %q), or empty for whatever "+
			"the bucket does by default",
			kitchenv1alpha1.ServerSideEncryptionAES256, kitchenv1alpha1.ServerSideEncryptionKMS, encryption)
		return
	}

	hasKey := body.S3.AccessKeyID != "" || body.S3.SecretAccessKey != ""
	switch {
	case hasKey && (body.S3.AccessKeyID == "" || body.S3.SecretAccessKey == ""):
		badRequest(w, "accessKeyId and secretAccessKey go together: half a key pair is a destination "+
			"that cannot authenticate")
		return
	case hasKey && body.S3.AmbientCredentials:
		badRequest(w, "ambientCredentials means this destination authenticates with the credential the "+
			"pod already has, so it takes no accessKeyId and secretAccessKey")
		return
	}

	base := kitchen.DeepCopy()
	destination := &kitchenv1alpha1.BackupDestination{
		Type: kitchenv1alpha1.BackupDestinationS3,
		S3: &kitchenv1alpha1.S3Destination{
			Bucket:               strings.TrimSpace(body.S3.Bucket),
			Prefix:               strings.Trim(strings.TrimSpace(body.S3.Prefix), "/"),
			Region:               strings.TrimSpace(body.S3.Region),
			Endpoint:             strings.TrimSpace(body.S3.Endpoint),
			ForcePathStyle:       body.S3.ForcePathStyle,
			ServerSideEncryption: body.S3.ServerSideEncryption,
			KMSKeyID:             strings.TrimSpace(body.S3.KMSKeyID),
		},
	}

	// What happens to the credential, in the three cases that are actually
	// different: a new key is stored, the destination is moved onto the
	// ambient chain, or the request said nothing and whatever is stored stays.
	switch {
	case hasKey:
		if err := s.writeCredentialsSecret(req, BackupDestinationSecretName, map[string][]byte{
			backup.CredentialKeyAccessKeyID:     []byte(body.S3.AccessKeyID),
			backup.CredentialKeySecretAccessKey: []byte(body.S3.SecretAccessKey),
		}, corev1.SecretTypeOpaque); err != nil {
			s.writeError(w, err)
			return
		}
		destination.S3.CredentialsSecretRef = &kitchenv1alpha1.LocalObjectReference{
			Name: BackupDestinationSecretName,
		}
	case body.S3.AmbientCredentials:
		if err := s.removeBackupCredential(req); err != nil {
			s.writeError(w, err)
			return
		}
	default:
		if existing := existingDestinationCredential(kitchen); existing != nil {
			destination.S3.CredentialsSecretRef = existing
		}
	}

	kitchen.Spec.Backup.Destination = destination
	if !s.recorded(w, req, audit.Transition{
		Object:    kitchen,
		Kind:      audit.KindKitchen,
		Operation: clickhouse.AuditUpdate,
		Reason: "the platform's backup destination was set to " + backup.Describe(destination) +
			", which makes that bucket this cluster's root credential store",
		Details: map[string]any{
			"change":      "backupDestination",
			"destination": backup.Describe(destination),
			"credential":  credentialKind(destination),
		},
	}) {
		return
	}
	if err := s.Client.Patch(ctx, kitchen, client.MergeFrom(base)); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("the platform's backup destination was set through the api",
		"destination", backup.Describe(destination), "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newBackupScheduleView(kitchen))
}

// deleteBackupDestination removes the destination and the credential with it.
//
// A schedule still in place is refused rather than left pointing at nothing:
// admission would refuse the write anyway, and answering here says which
// field to clear first instead of handing back a CEL rule's message.
func (s *Server) deleteBackupDestination(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if kitchen.Spec.Backup.Destination == nil {
		writeJSON(w, http.StatusOK, newBackupScheduleView(kitchen))
		return
	}
	if kitchen.Spec.Backup.Schedule != "" {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"this platform is scheduled to back itself up at %q, and a schedule with nowhere to write "+
				"to is refused. Clear the schedule first — PATCH /settings with backupSchedule: \"\" — "+
				"and then remove the destination.", kitchen.Spec.Backup.Schedule)})
		return
	}

	base := kitchen.DeepCopy()
	described := backup.Describe(kitchen.Spec.Backup.Destination)
	kitchen.Spec.Backup.Destination = nil
	// The retention goes with it. It prunes what is at the destination, so
	// with no destination it overrides nothing — and admission refuses the
	// pair, so leaving it behind would turn this route's success into the
	// API server's rejection.
	kitchen.Spec.Backup.Retention = kitchenv1alpha1.BackupRetentionSpec{}
	if !s.recorded(w, req, audit.Transition{
		Object:    kitchen,
		Kind:      audit.KindKitchen,
		Operation: clickhouse.AuditUpdate,
		Reason:    "the platform's backup destination " + described + " was removed",
		Details:   map[string]any{"change": "backupDestination", "destination": described},
	}) {
		return
	}
	if err := s.removeBackupCredential(req); err != nil {
		s.writeError(w, err)
		return
	}
	if err := s.Client.Patch(ctx, kitchen, client.MergeFrom(base)); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("the platform's backup destination was removed through the api",
		"destination", described, "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newBackupScheduleView(kitchen))
}

// removeBackupCredential deletes the Secret this API wrote for the
// destination, and only that one. A Secret something else put there — an
// external secrets sync, a hand-written manifest — carries no managed-by
// label and is left exactly where it is.
func (s *Server) removeBackupCredential(req *http.Request) error {
	ctx := req.Context()
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: BackupDestinationSecretName}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if secret.Labels[managedByLabelKey] != managedByLabelValue {
		return nil
	}
	if err := s.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// existingDestinationCredential is the Secret the destination already names,
// where it names one.
func existingDestinationCredential(kitchen *kitchenv1alpha1.Kitchen) *kitchenv1alpha1.LocalObjectReference {
	destination := kitchen.Spec.Backup.Destination
	if destination == nil || destination.S3 == nil {
		return nil
	}
	return destination.S3.CredentialsSecretRef
}

// credentialKind says how a destination authenticates, for the audit record.
func credentialKind(destination *kitchenv1alpha1.BackupDestination) string {
	if destination.S3 != nil && destination.S3.CredentialsSecretRef != nil {
		return backupCredentialStored
	}
	return backupCredentialAmbient
}

// backupObjectView is one object at the destination.
type backupObjectView struct {
	Key      string    `json:"key"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	// Archive is whether this is an archive this platform wrote, and so
	// whether retention would ever delete it. A bucket may hold other things
	// and they are listed too, precisely so that nobody has to wonder what
	// pruning would touch.
	Archive bool `json:"archive"`
}

// backupRunsView is what the destination holds.
type backupRunsView struct {
	Destination string             `json:"destination"`
	Objects     []backupObjectView `json:"objects"`
	// Truncated says the listing was cut at maxBackupObjects.
	Truncated bool `json:"truncated,omitempty"`
}

// listBackupRuns answers what is actually at the destination.
//
// It reads the bucket rather than the platform's own status on purpose. The
// status says what the last run believed it did; this says what is there now,
// which is the only thing a recovery can use.
func (s *Server) listBackupRuns(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	spec := kitchen.Spec.Backup.Destination
	if spec == nil {
		badRequest(w, "this platform has no backup destination, so there is nowhere for archives to be. "+
			"Set one with PUT /api/v1/platform/backup/destination, or on the Backup screen.")
		return
	}

	// The uncached reader: the destination's credential is a Secret the
	// manager keeps no informer over.
	target, err := backup.Open(ctx, s.reader(), s.Namespace, spec)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorBody{Error: err.Error()})
		return
	}
	objects, err := target.List(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorBody{Error: fmt.Sprintf(
			"the backup destination could not be listed: %s", err)})
		return
	}

	view := backupRunsView{Destination: target.String(), Objects: []backupObjectView{}}
	sort.Slice(objects, func(i, j int) bool {
		if !objects[i].Modified.Equal(objects[j].Modified) {
			return objects[i].Modified.After(objects[j].Modified)
		}
		return objects[i].Key > objects[j].Key
	})
	if len(objects) > maxBackupObjects {
		objects = objects[:maxBackupObjects]
		view.Truncated = true
	}
	for _, object := range objects {
		view.Objects = append(view.Objects, backupObjectView{
			Key:      object.Key,
			Size:     object.Size,
			Modified: object.Modified,
			Archive:  backup.IsArchiveName(path.Base(object.Key)),
		})
	}
	writeJSON(w, http.StatusOK, view)
}

// backupRunView names a run that has been started.
type backupRunView struct {
	// Job is the object doing the work, so that an operator can follow it.
	Job string `json:"job"`
	// Destination it is writing to.
	Destination string `json:"destination"`
}

// createBackupRun takes a backup now, to the destination, without downloading
// anything.
//
// This is what makes a destination testable on the day it is configured
// rather than at 02:00 six weeks later, and it is the reason the route exists
// at all. Nothing from the request reaches the Job: the pod template is the
// CronJob's own, copied.
func (s *Server) createBackupRun(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if !kitchen.Spec.Backup.Configured() {
		badRequest(w, "this platform has no scheduled backup to run: set a schedule and a destination "+
			"first. Taking a one-off archive into your browser is POST /api/v1/platform/backup.")
		return
	}

	cron := &batchv1.CronJob{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: controller.BackupCronJobName}
	if err := s.Client.Get(ctx, key, cron); err != nil {
		if apierrors.IsNotFound(err) {
			badRequest(w, "the scheduled backup has not been created yet — the platform's BackupReady "+
				"condition says why. It is written by the operator, a moment after the schedule is set.")
			return
		}
		s.writeError(w, err)
		return
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			// A generated suffix rather than a timestamp, for the reason a
			// scheduled process's manual run has one: two people pressing the
			// button in the same second each get the run they asked for.
			GenerateName: controller.BackupCronJobName + "-manual-",
			Namespace:    s.Namespace,
			Labels:       cron.Spec.JobTemplate.Labels,
			Annotations:  cron.Spec.JobTemplate.Annotations,
		},
		Spec: *cron.Spec.JobTemplate.Spec.DeepCopy(),
	}
	// The CronJob's history limits collect the Jobs it owns; this one is
	// owned by nobody, so it is given a TTL that collects it.
	job.Spec.TTLSecondsAfterFinished = ptr.To(int32(backupRunTTLSeconds))

	described := backup.Describe(kitchen.Spec.Backup.Destination)
	if !s.recorded(w, req, audit.Transition{
		Object:    kitchen,
		Kind:      audit.KindKitchen,
		Operation: clickhouse.AuditExport,
		Reason: "the platform's state was exported to " + described + " by hand: every custom resource, " +
			"every secret in " + controller.PlatformNamespace + ", and the identity provider's database",
		Details: map[string]any{"platformVersion": s.Version, "destination": described},
	}) {
		return
	}
	if err := s.Client.Create(ctx, job); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("a platform backup was started by hand through the api",
		"job", job.Name, "destination", described, "caller", callerName(caller))
	writeJSON(w, http.StatusAccepted, backupRunView{Job: job.Name, Destination: described})
}
