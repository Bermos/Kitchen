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

// Package receiver implements the git webhook receiver: the HTTP endpoint
// providers deliver push and pull-request events to. It is the entry point of
// the whole pipeline — a verified event becomes a Build CR, and the
// controllers take it from there.
package receiver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

const maxBodySize = 10 << 20 // 10 MiB

// The normalized event names every provider's headers are mapped onto, and
// the pull request actions the receiver acts on. GitLab's own spellings are
// folded onto these in webhookEvent and dispatchGitLab.
const (
	eventPush        = "push"
	eventPullRequest = "pull_request"
	eventPing        = "ping"

	actionOpened   = "opened"
	actionReopened = "reopened"
	actionClosed   = "closed"
	// GitHub spells a pull request's new commits "synchronize"; Gitea spells
	// the same thing "synchronized". Missing the second one is a preview that
	// never updates after the branch that opened it moves.
	actionSynchronize  = "synchronize"
	actionSynchronized = "synchronized"
)

// GitWebhookReceiver serves POST /webhooks/git/<connection> and creates Build
// CRs for verified events. It runs as a manager Runnable on every replica
// (webhook delivery does not need leader election).
type GitWebhookReceiver struct {
	Client client.Client
	// Namespace where Kitchen CRs (Projects, webhook secrets) live.
	Namespace string
	// BindAddr for the HTTP server, e.g. ":8090".
	BindAddr string
	// Activity feeds the dashboard's recent-activity feed, so that a pull
	// request the platform declined to build is visible somewhere other than
	// on the pull request itself. May be nil.
	Activity *activity.Recorder
	// Factory resolves the git provider a refusal is reported through.
	// Defaults to gitprovider.Default; tests inject fakes.
	Factory gitprovider.Factory
}

// Start implements manager.Runnable.
func (r *GitWebhookReceiver) Start(ctx context.Context) error {
	server := &http.Server{
		Addr:              r.BindAddr,
		Handler:           r.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logf.Log.WithName("git-webhook-receiver").Info("starting git webhook receiver", "addr", r.BindAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every replica
// should accept webhook deliveries.
func (r *GitWebhookReceiver) NeedLeaderElection() bool { return false }

func (r *GitWebhookReceiver) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/git/{connection}", r.handleGitEvent)
	return mux
}

// pushPayload is the subset of GitHub's push event the receiver needs.
type pushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	HeadCommit struct {
		Message string `json:"message"`
		Author  struct {
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"author"`
	} `json:"head_commit"`
}

// prPayload is the subset of GitHub's pull_request event the receiver needs.
// Gitea's is the same shape, down to `head.repo.full_name`, which is why one
// struct serves both.
type prPayload struct {
	Action      string `json:"action"`
	Number      int32  `json:"number"`
	PullRequest struct {
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
			// Repo is where the head commit actually lives, and is the whole
			// of issue #422: a pull request opened from a fork carries a
			// different repository here, and every provider sends it. Not
			// decoding it is how a stranger's commit came to be built with
			// the project's credentials and previewed with its secrets — the
			// head SHA of a fork's request is reachable in the base
			// repository through refs/pull/N/head, so nothing downstream
			// could tell the difference.
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type gitlabPushPayload struct {
	Ref      string `json:"ref"`
	After    string `json:"after"`
	User     string `json:"user_name"`
	Username string `json:"user_username"`
	Project  struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	Commits []struct {
		Message string `json:"message"`
		Author  struct {
			Name     string `json:"name"`
			Username string `json:"username"`
		} `json:"author"`
	} `json:"commits"`
}

type gitlabMRPayload struct {
	ObjectAttributes struct {
		Action       string `json:"action"`
		State        string `json:"state"`
		IID          int32  `json:"iid"`
		SourceBranch string `json:"source_branch"`
		// OldRev is set only when the update moved the source branch. Every
		// other edit to a merge request — a label, a title, an assignee —
		// arrives as the same "update" action with this empty.
		OldRev     string `json:"oldrev"`
		LastCommit struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"last_commit"`
		// GitLab says where a merge request's source branch lives with a
		// numeric project id rather than a path: a fork is a source project
		// that is not the target project (#422). The path is carried
		// alongside so a refusal can name the fork, but the *decision* is the
		// ids, because they are what GitLab guarantees.
		SourceProjectID int64 `json:"source_project_id"`
		TargetProjectID int64 `json:"target_project_id"`
		Source          struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"source"`
	} `json:"object_attributes"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	User struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"user"`
}

type giteaPushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	HeadCommit struct {
		Message string `json:"message"`
		Author  struct {
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"author"`
	} `json:"head_commit"`
	Pusher struct {
		Login    string `json:"login"`
		FullName string `json:"full_name"`
	} `json:"pusher"`
}

func (r *GitWebhookReceiver) handleGitEvent(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	log := logf.Log.WithName("git-webhook-receiver")
	connection := req.PathValue("connection")

	body, err := io.ReadAll(io.LimitReader(req.Body, maxBodySize))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// A ping is answered before anything is resolved. GitHub sends one the
	// moment a webhook is registered — which is before the Project that owns
	// it is reconciled — and the platform's own liveness probe sends one at
	// an address no Connection exists for at all.
	if isPing(req.Header) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
		return
	}

	providerName, err := r.connectionProvider(ctx, connection)
	if apierrors.IsNotFound(err) {
		// A delivery for a Connection that is gone has no project behind it
		// either. Answering 202 rather than an error keeps the provider from
		// disabling the webhook over it, exactly as an unmatched repository
		// already does.
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, "no connection %s", connection)
		return
	}
	if err != nil {
		log.Error(err, "failed to load connection", "connection", connection)
		http.Error(w, "failed to load connection", http.StatusInternalServerError)
		return
	}
	event := webhookEvent(providerName, req.Header)
	if event != eventPush && event != eventPullRequest {
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, "event %q ignored", event)
		return
	}

	repo, err := repoFullName(providerName, event, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	projects, err := r.projectsFor(ctx, connection, repo)
	if err != nil {
		http.Error(w, "failed to list projects", http.StatusInternalServerError)
		return
	}
	if len(projects) == 0 {
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, "no project for repository %s on connection %s", repo, connection)
		return
	}

	var created []string
	verified := false
	for i := range projects {
		project := &projects[i]
		secret, err := r.webhookSecret(ctx, project)
		if err != nil {
			log.Error(err, "no webhook secret", "project", project.Name)
			continue
		}
		if !verifySignature(providerName, req.Header, body, secret) {
			log.Info("signature verification failed", "project", project.Name, "repo", repo)
			continue
		}
		verified = true

		names, err := r.dispatch(ctx, project, providerName, event, body)
		if err != nil {
			log.Error(err, "failed to handle event", "project", project.Name, "event", event)
			http.Error(w, "failed to handle event", http.StatusInternalServerError)
			return
		}
		created = append(created, names...)
	}

	if !verified {
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"builds": created})
}

// dispatch handles a verified event for one project and returns the names of
// the Builds it created, or the ones an event told it something new about.
func (r *GitWebhookReceiver) dispatch(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	providerName string,
	event string,
	body []byte,
) ([]string, error) {
	switch providerName {
	case gitprovider.ProviderGitHub:
		return r.dispatchGitHub(ctx, project, event, body)
	case gitprovider.ProviderGitLab:
		return r.dispatchGitLab(ctx, project, event, body)
	case gitprovider.ProviderGitea:
		return r.dispatchGitea(ctx, project, event, body)
	default:
		return nil, nil
	}
}

func (r *GitWebhookReceiver) dispatchGitHub(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	event string,
	body []byte,
) ([]string, error) {
	switch event {
	case eventPush:
		payload := pushPayload{}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		if payload.Deleted || payload.After == "" || strings.Trim(payload.After, "0") == "" {
			return nil, nil // branch deletion
		}
		branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
		if !strings.HasPrefix(payload.Ref, "refs/heads/") {
			return nil, nil // tag or other ref
		}
		author := payload.HeadCommit.Author.Username
		if author == "" {
			author = payload.HeadCommit.Author.Name
		}
		return r.createBuild(ctx, project, pushRevision(
			payload.After, branch, payload.HeadCommit.Message, author))

	case eventPullRequest:
		payload := prPayload{}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		switch payload.Action {
		case actionOpened, actionSynchronize, actionReopened:
			return r.pullRequestBuild(ctx, project, kitchenv1alpha1.GitRevision{
				SHA:         payload.PullRequest.Head.SHA,
				Branch:      payload.PullRequest.Head.Ref,
				PullRequest: &payload.Number,
				ForkRepo:    forkRepoOf(project, payload.PullRequest.Head.Repo.FullName),
			})
		case actionClosed:
			// TODO: honor previews.ttlAfterClosed instead of immediate teardown.
			env := &kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{
				Name:      controller.PreviewEnvironmentName(project.Name, payload.Number),
				Namespace: project.Namespace,
			}}
			if err := r.Client.Delete(ctx, env); err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			}
			return nil, nil
		default:
			return nil, nil
		}
	}
	return nil, nil
}

func (r *GitWebhookReceiver) dispatchGitLab(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	event string,
	body []byte,
) ([]string, error) {
	switch event {
	case eventPush:
		payload := gitlabPushPayload{}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		if payload.After == "" || strings.Trim(payload.After, "0") == "" {
			return nil, nil
		}
		if !strings.HasPrefix(payload.Ref, "refs/heads/") {
			return nil, nil
		}
		branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
		message := ""
		author := prefer(payload.Username, payload.User)
		if n := len(payload.Commits); n > 0 {
			last := payload.Commits[n-1]
			message = last.Message
			if last.Author.Username != "" {
				author = last.Author.Username
			} else if last.Author.Name != "" {
				author = last.Author.Name
			}
		}
		return r.createBuild(ctx, project, pushRevision(payload.After, branch, message, author))
	case eventPullRequest:
		payload := gitlabMRPayload{}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		attrs := payload.ObjectAttributes
		action := strings.ToLower(attrs.Action)
		// GitLab's one "update" action covers every edit a merge request can
		// take, and only the ones that moved the source branch carry oldrev.
		// Building on the rest would redeploy a preview because somebody
		// relabelled it.
		if (action == "update" || action == "updated") && attrs.OldRev == "" {
			return nil, nil
		}
		switch action {
		case "open", actionOpened, "reopen", actionReopened, "update", "updated":
			subject, commitBody := kitchenv1alpha1.SplitCommitMessage(attrs.LastCommit.Message)
			return r.pullRequestBuild(ctx, project, kitchenv1alpha1.GitRevision{
				SHA:         attrs.LastCommit.ID,
				Branch:      attrs.SourceBranch,
				Message:     subject,
				Body:        commitBody,
				Author:      prefer(payload.User.Username, payload.User.Name),
				PullRequest: &attrs.IID,
				ForkRepo:    gitlabForkRepo(attrs.SourceProjectID, attrs.TargetProjectID, attrs.Source.PathWithNamespace),
			})
		case "close", actionClosed, "merge", "merged":
			env := &kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{
				Name:      controller.PreviewEnvironmentName(project.Name, attrs.IID),
				Namespace: project.Namespace,
			}}
			if err := r.Client.Delete(ctx, env); err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			}
		}
	}
	return nil, nil
}

func (r *GitWebhookReceiver) dispatchGitea(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	event string,
	body []byte,
) ([]string, error) {
	switch event {
	case eventPush:
		payload := giteaPushPayload{}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		if payload.After == "" || strings.Trim(payload.After, "0") == "" {
			return nil, nil
		}
		if !strings.HasPrefix(payload.Ref, "refs/heads/") {
			return nil, nil
		}
		author := prefer(payload.HeadCommit.Author.Username, payload.Pusher.Login)
		author = prefer(author, payload.HeadCommit.Author.Name)
		author = prefer(author, payload.Pusher.FullName)
		branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
		return r.createBuild(ctx, project, pushRevision(
			payload.After, branch, payload.HeadCommit.Message, author))
	case eventPullRequest:
		payload := prPayload{}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		switch payload.Action {
		case actionOpened, actionSynchronized, actionSynchronize, actionReopened:
			return r.pullRequestBuild(ctx, project, kitchenv1alpha1.GitRevision{
				SHA:         payload.PullRequest.Head.SHA,
				Branch:      payload.PullRequest.Head.Ref,
				PullRequest: &payload.Number,
				ForkRepo:    forkRepoOf(project, payload.PullRequest.Head.Repo.FullName),
			})
		case actionClosed:
			env := &kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{
				Name:      controller.PreviewEnvironmentName(project.Name, payload.Number),
				Namespace: project.Namespace,
			}}
			if err := r.Client.Delete(ctx, env); err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			}
		}
	}
	return nil, nil
}

func (r *GitWebhookReceiver) createBuild(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	revision kitchenv1alpha1.GitRevision,
) ([]string, error) {
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kitchenv1alpha1.BuildNameFor(project.Name, revision.SHA),
			Namespace: project.Namespace,
			Labels:    map[string]string{kitchenv1alpha1.ProjectLabel: project.Name},
		},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			Git:        revision,
		},
	}
	if err := r.Client.Create(ctx, build); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.recordPullRequest(ctx, build.Namespace, build.Name, revision.PullRequest)
		}
		return nil, err
	}
	return []string{build.Name}, nil
}

// pushRevision is a push's revision: the whole commit message split into the
// two things the platform shows — the subject in every row, the body behind
// it — and no pull request and no fork, because a push event is a push to the
// project's own repository by definition. No provider delivers a fork's pushes
// to the base repository's webhook; verified against GitHub, whose `push`
// event fires on the repository the branch is in, which for a fork is the
// fork's own webhook and not this one.
func pushRevision(sha, branch, message, author string) kitchenv1alpha1.GitRevision {
	subject, commitBody := kitchenv1alpha1.SplitCommitMessage(message)
	return kitchenv1alpha1.GitRevision{
		SHA:     sha,
		Branch:  branch,
		Message: subject,
		Body:    commitBody,
		Author:  author,
	}
}

// pullRequestBuild is createBuild with the fork gate in front of it (#422).
//
// A pull request whose head is in the project's own repository is built the
// way it always was. One whose head is somewhere else is a stranger's code,
// and what it gets is the project's `spec.previews.forks` bounded by the
// platform's `spec.previews.forksMax`:
//
//   - `none`, the default: no Build at all, and the pull request is told so
//     where its author is looking — a commit status and the preview comment.
//     Refusing here rather than in the build controller is the point: a Build
//     that exists is a build pod holding the project's registry credential.
//   - `build`: the Build is created and records where the head came from; the
//     build controller then declines to give it a preview, so nothing the
//     project configured reaches the fork's code.
//   - `full`: exactly what the project's own branch gets.
func (r *GitWebhookReceiver) pullRequestBuild(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	revision kitchenv1alpha1.GitRevision,
) ([]string, error) {
	if !revision.IsFork() {
		return r.createBuild(ctx, project, revision)
	}
	policy := controller.ForkPolicyFor(ctx, r.Client, project)
	if policy.BuildsForks() {
		return r.createBuild(ctx, project, revision)
	}
	r.refuseFork(ctx, project, revision, policy)
	return nil, nil
}

// refuseFork says, in the three places the platform says things, that this
// pull request got nothing: on the request itself, in the activity feed, and
// in the operator's log.
//
// It is best effort by construction — a provider that cannot be reached does
// not change what was refused — so nothing here returns an error. The delivery
// is still a 202: the platform read it, understood it and decided.
func (r *GitWebhookReceiver) refuseFork(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	revision kitchenv1alpha1.GitRevision,
	policy kitchenv1alpha1.ForkPolicy,
) {
	pullRequest := int32(0)
	if revision.PullRequest != nil {
		pullRequest = *revision.PullRequest
	}
	reason := controller.ForkRefusalMessage(project, policy, revision.ForkRepo)
	logf.Log.WithName("git-webhook-receiver").Info("refused a pull request from a fork",
		"project", project.Name, "pullRequest", pullRequest,
		"fork", revision.ForkRepo, "commit", revision.SHA, "forks", policy)
	controller.ForkReporter{Client: r.Client, Factory: r.Factory}.
		ReportForkRefused(ctx, project, revision, pullRequest, reason)
	r.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventPreviewRefused,
		Project: project.Name,
		Message: reason,
	})
}

// forkRepoOf is where a GitHub or Gitea pull request's head lives, as the
// Build records it: empty when it is the project's own repository, and the
// head repository's full name when it is not.
//
// A payload that names no head repository is a fork, not the project's own.
// That is the only safe reading of a missing field here — the field is what
// this whole gate turns on, and a provider that stopped sending it, or a shape
// this receiver has not met, must fail closed rather than hand a stranger the
// project's credentials.
func forkRepoOf(project *kitchenv1alpha1.Project, headRepo string) string {
	if headRepo == "" {
		return kitchenv1alpha1.UnknownForkRepo
	}
	if strings.EqualFold(headRepo, project.Spec.Source.GitSource().Repo) {
		return ""
	}
	return headRepo
}

// gitlabForkRepo is the same question for GitLab, which answers it with
// numeric project ids rather than paths: a merge request whose source project
// is not its target project comes from a fork.
//
// A payload missing either id is treated as a fork for the reason forkRepoOf
// treats a missing name as one — and here it matters twice over, because two
// absent ids are equal, so reading them naively would call every malformed
// delivery the project's own.
func gitlabForkRepo(sourceID, targetID int64, sourcePath string) string {
	if sourceID != 0 && targetID != 0 && sourceID == targetID {
		return ""
	}
	if sourcePath == "" {
		return kitchenv1alpha1.UnknownForkRepo
	}
	return sourcePath
}

// recordPullRequest is what happens when the Build for this commit is already
// there.
//
// Most of the time that is a redelivery and there is nothing to do. The case
// that is not is a pull request event for a commit the platform first heard
// about as a push — which is the ordinary way a request is opened, since every
// provider delivers the push first and a branch is usually pushed before
// anybody opens a request for it. That delivery is not a repeat of anything:
// it carries the one fact the push could not, and dropping it is a preview
// environment that never appears.
//
// It is written as an annotation, patched rather than updated so it cannot
// lose a race with the reconciler writing status. The first request to claim a
// commit keeps it: the same head can belong to two open requests, and picking
// the later one would move a preview that already exists to a different
// request's name.
func (r *GitWebhookReceiver) recordPullRequest(
	ctx context.Context,
	namespace, name string,
	pullRequest *int32,
) ([]string, error) {
	if pullRequest == nil {
		return nil, nil // webhook redelivery of a push
	}
	build := &kitchenv1alpha1.Build{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, build); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil // deleted between the create and the read
		}
		return nil, err
	}
	if build.PullRequestNumber() != nil {
		return nil, nil // already associated, by this request or another
	}
	patch := client.MergeFrom(build.DeepCopy())
	if build.Annotations == nil {
		build.Annotations = map[string]string{}
	}
	build.Annotations[kitchenv1alpha1.PullRequestAnnotation] = strconv.FormatInt(int64(*pullRequest), 10)
	if err := r.Client.Patch(ctx, build, patch); err != nil {
		return nil, err
	}
	logf.Log.WithName("git-webhook-receiver").Info("associated an existing build with a pull request",
		"build", build.Name, "commit", build.Spec.Git.SHA, "pullRequest", *pullRequest)
	return []string{build.Name}, nil
}

// projectsFor returns the Projects bound to the Connection and repository.
func (r *GitWebhookReceiver) projectsFor(
	ctx context.Context,
	connection, repo string,
) ([]kitchenv1alpha1.Project, error) {
	list := &kitchenv1alpha1.ProjectList{}
	if err := r.Client.List(ctx, list, client.InNamespace(r.Namespace)); err != nil {
		return nil, err
	}
	var out []kitchenv1alpha1.Project
	for _, p := range list.Items {
		if p.Spec.Source.GitSource().ConnectionRef.Name == connection && strings.EqualFold(p.Spec.Source.GitSource().Repo, repo) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (r *GitWebhookReceiver) webhookSecret(ctx context.Context, project *kitchenv1alpha1.Project) (string, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: project.Namespace, Name: "kitchen-webhook-" + project.Name}
	if err := r.Client.Get(ctx, key, secret); err != nil {
		return "", err
	}
	value := string(secret.Data["secret"])
	if value == "" {
		return "", fmt.Errorf("webhook secret for project %q is empty", project.Name)
	}
	return value, nil
}

func (r *GitWebhookReceiver) connectionProvider(ctx context.Context, connection string) (string, error) {
	conn := &kitchenv1alpha1.Connection{}
	key := types.NamespacedName{Namespace: r.Namespace, Name: connection}
	if err := r.Client.Get(ctx, key, conn); err != nil {
		return "", err
	}
	return conn.Spec.Provider, nil
}

func repoFullName(providerName, event string, body []byte) (string, error) {
	if providerName == gitprovider.ProviderGitLab {
		envelope := struct {
			Project struct {
				PathWithNamespace string `json:"path_with_namespace"`
			} `json:"project"`
		}{}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return "", fmt.Errorf("invalid %s payload", event)
		}
		if envelope.Project.PathWithNamespace == "" {
			return "", fmt.Errorf("%s payload has no repository", event)
		}
		return envelope.Project.PathWithNamespace, nil
	}
	envelope := struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}{}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("invalid %s payload", event)
	}
	if envelope.Repository.FullName == "" {
		return "", fmt.Errorf("%s payload has no repository", event)
	}
	return envelope.Repository.FullName, nil
}

// isPing reports whether the delivery is a webhook handshake rather than a
// repository event. Only GitHub has one — GitLab's and Gitea's test buttons
// send a real push — so the other two headers are read for symmetry alone,
// and cost nothing.
func isPing(header http.Header) bool {
	for _, name := range []string{"X-GitHub-Event", "X-Gitea-Event", "X-Gitlab-Event"} {
		if strings.EqualFold(header.Get(name), eventPing) {
			return true
		}
	}
	return false
}

func webhookEvent(providerName string, header http.Header) string {
	switch providerName {
	case gitprovider.ProviderGitLab:
		switch header.Get("X-Gitlab-Event") {
		case "Push Hook":
			return eventPush
		case "Merge Request Hook":
			return eventPullRequest
		default:
			return ""
		}
	case gitprovider.ProviderGitea:
		return strings.ToLower(header.Get("X-Gitea-Event"))
	default:
		return strings.ToLower(header.Get("X-GitHub-Event"))
	}
}

func verifySignature(providerName string, header http.Header, body []byte, secret string) bool {
	switch providerName {
	case gitprovider.ProviderGitLab:
		return subtleEqual(header.Get("X-Gitlab-Token"), secret)
	case gitprovider.ProviderGitea:
		return verifyHMACSHA256(header.Get("X-Gitea-Signature"), body, secret)
	default:
		return verifyHMACSHA256(header.Get("X-Hub-Signature-256"), body, secret)
	}
}

func verifyHMACSHA256(header string, body []byte, secret string) bool {
	digest := strings.TrimSpace(header)
	if digest == "" {
		return false
	}
	if value, ok := strings.CutPrefix(strings.ToLower(digest), "sha256="); ok {
		digest = value
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(digest))
}

func subtleEqual(a, b string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	return hmac.Equal([]byte(a), []byte(b))
}

func prefer(first, fallback string) string {
	if first != "" {
		return first
	}
	return fallback
}
