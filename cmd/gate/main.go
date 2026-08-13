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
// useful to someone signed in to the platform. It ships in the operator's
// image and is deployed by the Helm chart; the operator registers its OAuth
// client and points the protected routes at it.
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Bermos/Kitchen/internal/previewgate"
)

// shutdownGrace is how long in-flight requests get once a signal arrives.
const shutdownGrace = 10 * time.Second

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

	server := previewgate.NewServer(cfg, nil, log)
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
		"issuer", cfg.Issuer, "gateHost", cfg.GateHost())

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
