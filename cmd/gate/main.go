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

// Command gate runs Kitchen's forward-auth gate: the component protected
// preview environments are routed through, so that a preview URL is only
// useful to someone on the project it belongs to. It ships in the operator's
// image and is deployed by the Helm chart; the operator registers its OAuth
// client and points the protected routes at it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/previewgate"
)

const (
	// shutdownGrace is how long in-flight requests get once a signal arrives.
	shutdownGrace = 10 * time.Second

	// cacheSyncGrace bounds the wait for the first list of Projects and the
	// Kitchen singleton. Serving before they have arrived would mean refusing
	// members of every project for as long as the sync took — correct, since
	// the gate fails closed, but indistinguishable from a broken platform. So
	// the listeners do not open until the cache has answered once.
	cacheSyncGrace = 2 * time.Minute
)

// scheme carries only Kitchen's own kinds. The gate reads two of them and
// writes nothing at all.
var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(kitchenv1alpha1.AddToScheme(scheme))
}

func main() {
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := previewgate.DefaultLogger()

	cfg, err := previewgate.LoadConfig(os.LookupEnv)
	if err != nil {
		log.Error(err, "the gate is not configured")
		os.Exit(1)
	}

	// Kitchen's custom resources all live in one namespace, which is the one
	// the gate itself runs in. The chart sets POD_NAMESPACE from the downward
	// API; the fallback is the namespace the platform is compiled to install
	// into, as the manager's own does.
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = "kitchen-system"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Membership is resolved against a cache of the platform's own objects,
	// not by asking the REST API: a preview that closed while the API was
	// restarting would be an outage in something the gate can decide for
	// itself, and the rule has exactly one implementation either way
	// (internal/access). It is also in the request path of every protected
	// preview, so it has to cost a map lookup rather than a round trip.
	directory, err := startDirectory(ctx, namespace, log)
	if err != nil {
		// Without it the gate can admit nobody, and a pod that says so and
		// restarts is a clearer report than one that serves refusals forever.
		log.Error(err, "the gate cannot read the platform's projects")
		os.Exit(1)
	}

	server := previewgate.NewServer(cfg, nil, directory, log)
	proxy := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
	}
	health := &http.Server{
		Addr:              cfg.HealthAddr,
		Handler:           server.HealthHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	failed := make(chan error, 2)
	for _, listener := range []*http.Server{proxy, health} {
		go func(listener *http.Server) {
			if err := listener.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failed <- err
			}
		}(listener)
	}
	log.Info("listening",
		"addr", cfg.Addr, "health", cfg.HealthAddr,
		"issuer", cfg.Issuer, "gateHost", cfg.GateHost(), "namespace", namespace)

	crashed := false
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-failed:
		log.Error(err, "the gate stopped serving")
		crashed = true
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = proxy.Shutdown(shutdownCtx)
	_ = health.Shutdown(shutdownCtx)
	if crashed {
		os.Exit(1)
	}
}

// startDirectory builds the gate's read-only view of the platform and waits
// for it to be populated before returning.
//
// The cache watches Projects in the platform namespace and the cluster-scoped
// Kitchen singleton, and nothing else — that is the whole of the gate's
// ServiceAccount too, which the chart binds to a role with get, list and
// watch on those two kinds.
func startDirectory(ctx context.Context, namespace string, log logr.Logger) (previewgate.Directory, error) {
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}

	platform, err := cluster.New(restConfig, func(o *cluster.Options) {
		o.Scheme = scheme
		o.Cache = cache.Options{
			// Projects are namespaced and all of them are here. Kitchen is
			// cluster-scoped, so it is unaffected by this and watched whole —
			// there is one of it.
			ByObject: map[client.Object]cache.ByObject{
				&kitchenv1alpha1.Project{}: {
					Namespaces: map[string]cache.Config{namespace: {}},
				},
			},
		}
	})
	if err != nil {
		return nil, err
	}

	go func() {
		if err := platform.Start(ctx); err != nil {
			log.Error(err, "the platform cache stopped")
		}
	}()

	// The informers are asked for by hand rather than left to start on the
	// first request. A controller-runtime cache creates them lazily, so
	// WaitForCacheSync on an untouched cache succeeds without having listed
	// anything — and the first preview request of the day would be the one
	// that discovers the ServiceAccount cannot read Projects.
	syncCtx, cancel := context.WithTimeout(ctx, cacheSyncGrace)
	defer cancel()
	for _, kind := range []client.Object{&kitchenv1alpha1.Project{}, &kitchenv1alpha1.Kitchen{}} {
		if _, err := platform.GetCache().GetInformer(syncCtx, kind); err != nil {
			return nil, fmt.Errorf("watching %T did not start within %s "+
				"(the gate's ServiceAccount needs get, list and watch on projects and kitchens in %s): %w",
				kind, cacheSyncGrace, kitchenv1alpha1.GroupVersion.Group, err)
		}
	}
	return previewgate.CachedDirectory{Reader: platform.GetClient(), Namespace: namespace}, nil
}
