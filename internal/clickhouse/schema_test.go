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

package clickhouse

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeStore records the statements it is sent and answers the one query the
// schema code reads back.
type fakeStore struct {
	server   *httptest.Server
	queries  []string
	engine   string
	failWith string
}

func newFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	store := &fakeStore{}
	store.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		query := string(body)
		store.queries = append(store.queries, query)

		if store.failWith != "" {
			http.Error(w, store.failWith, http.StatusBadRequest)
			return
		}
		if strings.Contains(query, "FROM system.tables") {
			_, _ = io.WriteString(w, store.engine+"\n")
			return
		}
	}))
	t.Cleanup(store.server.Close)
	return store
}

func (s *fakeStore) client(t *testing.T) *Client {
	t.Helper()
	endpoint, err := url.Parse(s.server.URL)
	if err != nil {
		t.Fatalf("parsing the test server URL: %v", err)
	}
	return New(Config{
		Host:     endpoint.Hostname(),
		HTTPPort: endpoint.Port(),
		Database: "kitchen",
		Username: "kitchen",
		Password: "hunter2",
	})
}

func (s *fakeStore) sent(fragment string) bool {
	for _, query := range s.queries {
		if strings.Contains(query, fragment) {
			return true
		}
	}
	return false
}

func TestEnsureLogsSchemaCreatesTheTable(t *testing.T) {
	store := newFakeStore(t)
	// A table that does not exist yet reports no engine at all.
	store.engine = ""

	if err := store.client(t).EnsureLogsSchema(context.Background(), 14); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}

	for _, want := range []string{
		"CREATE DATABASE IF NOT EXISTS `kitchen`",
		"CREATE TABLE IF NOT EXISTS `kitchen`.`logs`",
		"ORDER BY (project, environment, timestamp)",
		"TTL toDateTime(timestamp) + toIntervalDay(14)",
	} {
		if !store.sent(want) {
			t.Errorf("expected a statement containing %q, got:\n%s", want, strings.Join(store.queries, "\n---\n"))
		}
	}
}

func TestEnsureLogsSchemaAltersTTLWhenRetentionChanges(t *testing.T) {
	store := newFakeStore(t)
	// The table exists, retaining 30 days.
	store.engine = "MergeTree PARTITION BY toDate(timestamp) ORDER BY (project, environment, timestamp) " +
		"TTL toDateTime(timestamp) + toIntervalDay(30)"

	if err := store.client(t).EnsureLogsSchema(context.Background(), 7); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}

	want := "ALTER TABLE `kitchen`.`logs` MODIFY TTL toDateTime(timestamp) + toIntervalDay(7)"
	if !store.sent(want) {
		t.Errorf("expected %q, got:\n%s", want, strings.Join(store.queries, "\n---\n"))
	}
}

func TestEnsureLogsSchemaLeavesAMatchingTTLAlone(t *testing.T) {
	store := newFakeStore(t)
	store.engine = "MergeTree TTL toDateTime(timestamp) + toIntervalDay(30)"

	if err := store.client(t).EnsureLogsSchema(context.Background(), 30); err != nil {
		t.Fatalf("EnsureLogsSchema: %v", err)
	}

	if store.sent("ALTER TABLE") {
		t.Errorf("the TTL already matched, no ALTER expected, got:\n%s", strings.Join(store.queries, "\n---\n"))
	}
}

func TestEnsureLogsSchemaRejectsNonsenseRetention(t *testing.T) {
	store := newFakeStore(t)

	if err := store.client(t).EnsureLogsSchema(context.Background(), 0); err == nil {
		t.Fatal("expected a retention of 0 days to be rejected")
	}
	if len(store.queries) != 0 {
		t.Errorf("expected no statements to be sent, got %d", len(store.queries))
	}
}

func TestEnsureLogsSchemaSurfacesStoreErrors(t *testing.T) {
	store := newFakeStore(t)
	store.failWith = "Code: 516. DB::Exception: kitchen: Authentication failed"

	err := store.client(t).EnsureLogsSchema(context.Background(), 30)
	if err == nil {
		t.Fatal("expected the store's error to surface")
	}
	if !strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("expected the store's own message in %q", err.Error())
	}
}

func TestQuerySendsCredentialsAndDatabase(t *testing.T) {
	var user, key, database string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user = r.Header.Get("X-ClickHouse-User")
		key = r.Header.Get("X-ClickHouse-Key")
		database = r.URL.Query().Get("database")
		_, _ = io.WriteString(w, "1")
	}))
	t.Cleanup(server.Close)

	endpoint, _ := url.Parse(server.URL)
	client := New(Config{
		Host: endpoint.Hostname(), HTTPPort: endpoint.Port(),
		Database: "kitchen", Username: "kitchen", Password: "hunter2",
	})

	answer, err := client.Query(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if answer != "1" {
		t.Errorf("answer = %q, want %q", answer, "1")
	}
	if user != "kitchen" || key != "hunter2" || database != "kitchen" {
		t.Errorf("sent user=%q key=%q database=%q", user, key, database)
	}
}

func TestConfigFromSecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kitchen-clickhouse", Namespace: "kitchen-system"},
		Data: map[string][]byte{
			SecretKeyHost:     []byte("kitchen-clickhouse.kitchen-system.svc"),
			SecretKeyHTTPPort: []byte("8123"),
			SecretKeyDatabase: []byte("kitchen"),
			SecretKeyUsername: []byte("kitchen"),
			SecretKeyPassword: []byte("hunter2"),
		},
	}

	cfg, err := ConfigFromSecret(secret)
	if err != nil {
		t.Fatalf("ConfigFromSecret: %v", err)
	}
	if cfg.Host != "kitchen-clickhouse.kitchen-system.svc" || cfg.HTTPPort != "8123" {
		t.Errorf("unexpected config %+v", cfg)
	}

	delete(secret.Data, SecretKeyHost)
	delete(secret.Data, SecretKeyUsername)
	_, err = ConfigFromSecret(secret)
	if err == nil {
		t.Fatal("expected missing keys to be reported")
	}
	if !strings.Contains(err.Error(), "host, username") {
		t.Errorf("expected both missing keys in %q", err.Error())
	}

	// A database name that is not an identifier would be interpolated into
	// DDL; it is rejected rather than quoted and hoped for.
	secret.Data[SecretKeyHost] = []byte("clickhouse")
	secret.Data[SecretKeyUsername] = []byte("kitchen")
	secret.Data[SecretKeyDatabase] = []byte("kitchen`; DROP DATABASE kitchen; --")
	if _, err := ConfigFromSecret(secret); err == nil {
		t.Fatal("expected an unusable database name to be rejected")
	}
}
