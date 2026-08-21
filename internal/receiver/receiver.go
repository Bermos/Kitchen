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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const maxBodySize = 10 << 20 // 10 MiB

// GitWebhookReceiver serves POST /webhooks/git/<connection> and creates Build
// CRs for verified events. It runs as a manager Runnable on every replica
// (webhook delivery does not need leader election).
type GitWebhookReceiver struct {
	Client client.Client
	// Namespace where Kitchen CRs (Projects, webhook secrets) live.
	Namespace string
	// BindAddr for the HTTP server, e.g. ":8090".
	BindAddr string
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
type prPayload struct {
	Action      string `json:"action"`
	Number      int32  `json:"number"`
	PullRequest struct {
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
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

	// TODO: event parsing is GitHub-shaped for now; route by the
	// Connection's provider once more git providers land.
	event := req.Header.Get("X-GitHub-Event")
	if event == "ping" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
		return
	}
	if event != "push" && event != "pull_request" {
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, "event %q ignored", event)
		return
	}

	repo, err := repoFullName(event, body)
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

	signature := req.Header.Get("X-Hub-Signature-256")
	var created []string
	verified := false
	for i := range projects {
		project := &projects[i]
		secret, err := r.webhookSecret(ctx, project)
		if err != nil {
			log.Error(err, "no webhook secret", "project", project.Name)
			continue
		}
		if !verifySignature(signature, body, secret) {
			log.Info("signature verification failed", "project", project.Name, "repo", repo)
			continue
		}
		verified = true

		names, err := r.dispatch(ctx, project, event, body)
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
// any Builds it created.
func (r *GitWebhookReceiver) dispatch(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	event string,
	body []byte,
) ([]string, error) {
	switch event {
	case "push":
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
		return r.createBuild(ctx, project, payload.After, branch, payload.HeadCommit.Message, author, nil)

	case "pull_request":
		payload := prPayload{}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		switch payload.Action {
		case "opened", "synchronize", "reopened":
			return r.createBuild(ctx, project,
				payload.PullRequest.Head.SHA, payload.PullRequest.Head.Ref, "", "", &payload.Number)
		case "closed":
			// TODO: honor previews.ttlAfterClosed instead of immediate teardown.
			env := &kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-pr-%d", project.Name, payload.Number),
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

func (r *GitWebhookReceiver) createBuild(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	sha, branch, message, author string,
	pullRequest *int32,
) ([]string, error) {
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kitchenv1alpha1.BuildNameFor(project.Name, sha),
			Namespace: project.Namespace,
			Labels:    map[string]string{kitchenv1alpha1.ProjectLabel: project.Name},
		},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			Git: kitchenv1alpha1.GitRevision{
				SHA:         sha,
				Branch:      branch,
				Message:     message,
				Author:      author,
				PullRequest: pullRequest,
			},
		},
	}
	if err := r.Client.Create(ctx, build); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, nil // webhook redelivery
		}
		return nil, err
	}
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
		if p.Spec.Source.ConnectionRef.Name == connection && strings.EqualFold(p.Spec.Source.Repo, repo) {
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

func repoFullName(event string, body []byte) (string, error) {
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

// verifySignature checks GitHub's X-Hub-Signature-256 HMAC over the body.
func verifySignature(header string, body []byte, secret string) bool {
	digest, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(digest))
}
