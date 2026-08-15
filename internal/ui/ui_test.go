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

package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bermos/Kitchen/internal/version"
)

func getConfig(t *testing.T, handler http.Handler) (*httptest.ResponseRecorder, Config) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/config.json", nil))
	cfg := Config{}
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &cfg); err != nil {
			t.Fatalf("decoding /config.json: %v", err)
		}
	}
	return recorder, cfg
}

func TestConfigCarriesTheBuiltVersion(t *testing.T) {
	original := version.Version
	version.Version = "1.2.3"
	t.Cleanup(func() { version.Version = original })

	// The resolver supplies what it reads off the Kitchen singleton, and
	// deliberately no version: the version belongs to the binary, so the
	// handler stamps it whatever the resolver returns.
	handler := Handler(func(context.Context) (Config, error) {
		return Config{Issuer: "https://auth.apps.example.com", ClientID: "kitchen-ui"}, nil
	})

	recorder, cfg := getConfig(t, handler)
	if recorder.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", recorder.Code)
	}
	if cfg.Version != "1.2.3" {
		t.Errorf("got version %q, want %q", cfg.Version, "1.2.3")
	}
	if cfg.Issuer != "https://auth.apps.example.com" {
		t.Errorf("the resolver's fields were lost: %+v", cfg)
	}
}

func TestConfigIsUnavailableBeforeThePlatformIsConfigured(t *testing.T) {
	handler := Handler(func(context.Context) (Config, error) {
		return Config{}, errors.New("no Kitchen singleton yet")
	})

	recorder, _ := getConfig(t, handler)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want 503", recorder.Code)
	}
}
