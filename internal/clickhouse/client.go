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

// Package clickhouse talks to the telemetry store over its HTTP interface.
// The operator only ever runs DDL and reads back small answers, so a plain
// net/http client is enough — no driver, no connection pool to babysit.
package clickhouse

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Secret keys the chart writes into the telemetry connection secret. The same
// shape is produced whether the chart runs ClickHouse or points at an
// external one.
const (
	SecretKeyHost     = "host"
	SecretKeyHTTPPort = "httpPort"
	SecretKeyDatabase = "database"
	SecretKeyUsername = "username"
	SecretKeyPassword = "password"
)

// identifierPattern is what may appear unquoted in the DDL the operator
// builds. The database name comes from a Secret, so it is checked rather
// than trusted.
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Config is a resolved connection to the telemetry store.
type Config struct {
	Host     string
	HTTPPort string
	Database string
	Username string
	Password string
}

// ConfigFromSecret reads the connection details the chart wrote.
func ConfigFromSecret(secret *corev1.Secret) (Config, error) {
	cfg := Config{
		Host:     string(secret.Data[SecretKeyHost]),
		HTTPPort: string(secret.Data[SecretKeyHTTPPort]),
		Database: string(secret.Data[SecretKeyDatabase]),
		Username: string(secret.Data[SecretKeyUsername]),
		Password: string(secret.Data[SecretKeyPassword]),
	}
	var missing []string
	for key, value := range map[string]string{
		SecretKeyHost:     cfg.Host,
		SecretKeyHTTPPort: cfg.HTTPPort,
		SecretKeyDatabase: cfg.Database,
		SecretKeyUsername: cfg.Username,
	} {
		if value == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return Config{}, fmt.Errorf("secret %s/%s is missing the keys: %s",
			secret.Namespace, secret.Name, strings.Join(missing, ", "))
	}
	if !identifierPattern.MatchString(cfg.Database) {
		return Config{}, fmt.Errorf("secret %s/%s holds an unusable database name %q",
			secret.Namespace, secret.Name, cfg.Database)
	}
	return cfg, nil
}

// endpoint is the HTTP address queries are posted to.
func (c Config) endpoint() string {
	return fmt.Sprintf("http://%s:%s/", c.Host, c.HTTPPort)
}

// Client runs statements against one ClickHouse.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a client with a timeout that keeps a wedged store from stalling
// a reconcile.
func New(cfg Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 15 * time.Second}}
}

// Exec runs a statement and discards its output.
func (c *Client) Exec(ctx context.Context, query string) error {
	_, err := c.Query(ctx, query)
	return err
}

// Query runs a statement and returns its response body, trimmed. Answers the
// operator asks for are single values or a handful of rows, so they are read
// whole.
func (c *Client) Query(ctx context.Context, query string) (string, error) {
	endpoint := c.cfg.endpoint() + "?" + url.Values{
		"database": {c.cfg.Database},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(query))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("X-ClickHouse-User", c.cfg.Username)
	req.Header.Set("X-ClickHouse-Key", c.cfg.Password)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("clickhouse at %s: %w", c.cfg.endpoint(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	// ClickHouse reports errors as the body of a non-2xx response; it is the
	// only useful diagnostic, so a bounded prefix of it goes into the error.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("clickhouse returned %s: %s",
			resp.Status, strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(string(body)), nil
}

// quoteIdentifier backtick-quotes a name that has already been checked
// against identifierPattern.
func quoteIdentifier(name string) string {
	return "`" + name + "`"
}

// quoteLiteral renders a string literal for the few places a value reaches
// the query text.
func quoteLiteral(value string) string {
	var b bytes.Buffer
	b.WriteByte('\'')
	for _, r := range value {
		switch r {
		case '\'', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}
