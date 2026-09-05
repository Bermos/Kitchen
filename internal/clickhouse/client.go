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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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

	// SecretKeyScheme says whether the store's HTTP interface answers in the
	// clear or over TLS. The chart writes `https` for the bundled ClickHouse,
	// which serves a certificate the operator requests from the platform's
	// own internal CA, and for an external store configured for it.
	//
	// An absent key is read as `http`, because a secret written by a chart
	// older than the certificate is describing a store that really does
	// answer in the clear. It is the one place plaintext survives, and it is
	// the chart's statement of fact rather than a fallback the client takes
	// on its own: nothing here ever downgrades a `https` connection.
	SecretKeyScheme = "scheme"

	// SecretKeyCAFile is where the PEM bundle that must sign the store's
	// certificate is mounted in this pod. It is a path rather than the bundle
	// itself because the bundle belongs to the operator, which mints it, and
	// the chart is what decides where a pod sees it — the same ConfigMap is
	// mounted into the telemetry agent under the same name.
	//
	// Empty means the host's own roots, which is what an external store with
	// a publicly trusted certificate wants. It never means "do not verify":
	// there is no setting for that anywhere in this package.
	SecretKeyCAFile = "caFile"

	// SecretKeyCertificateSecret names the Secret the store's own certificate
	// belongs in, and by naming it asks the operator to fill that Secret from
	// the platform's internal CA. It is the one key here the client never
	// reads: it is addressed to the operator, and it lives in this secret
	// because this secret is where everything about the connection is
	// written down.
	//
	// Absent means the operator issues nothing — an external store, whose
	// certificate is somebody else's to manage, or a bundled store somebody
	// has deliberately left in the clear.
	SecretKeyCertificateSecret = "certificateSecret"
)

// The two schemes the HTTP interface may be reached on.
const (
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"
)

const (
	// maxErrorBytes bounds how much of a failed query's diagnostic is kept.
	maxErrorBytes = 8 << 10
	// maxResponseBytes bounds a successful answer. Schema queries return a
	// handful of bytes; a page of log lines is the large case, and 16 MiB
	// is far past the ceiling the log query enforces on its own.
	maxResponseBytes = 16 << 20
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

	// Scheme is http or https. Empty is http, for a connection secret
	// written before the store had a certificate.
	Scheme string

	// CAFile is the PEM bundle the store's certificate is verified against.
	// Empty verifies against the host's roots. Verification itself is not
	// optional either way.
	CAFile string
}

// ConfigFromSecret reads the connection details the chart wrote.
func ConfigFromSecret(secret *corev1.Secret) (Config, error) {
	cfg := Config{
		Host:     string(secret.Data[SecretKeyHost]),
		HTTPPort: string(secret.Data[SecretKeyHTTPPort]),
		Database: string(secret.Data[SecretKeyDatabase]),
		Username: string(secret.Data[SecretKeyUsername]),
		Password: string(secret.Data[SecretKeyPassword]),
		Scheme:   string(secret.Data[SecretKeyScheme]),
		CAFile:   string(secret.Data[SecretKeyCAFile]),
	}
	if cfg.Scheme == "" {
		cfg.Scheme = SchemeHTTP
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
	if cfg.Scheme != SchemeHTTP && cfg.Scheme != SchemeHTTPS {
		return Config{}, fmt.Errorf("secret %s/%s asks for scheme %q; it is %s or %s",
			secret.Namespace, secret.Name, cfg.Scheme, SchemeHTTP, SchemeHTTPS)
	}
	return cfg, nil
}

// scheme is the connection's scheme, defaulted for a Config built in code
// rather than read from a secret.
func (c Config) scheme() string {
	if c.Scheme == "" {
		return SchemeHTTP
	}
	return c.Scheme
}

// endpoint is the HTTP address queries are posted to.
func (c Config) endpoint() string {
	return fmt.Sprintf("%s://%s:%s/", c.scheme(), c.Host, c.HTTPPort)
}

// Client runs statements against one ClickHouse.
type Client struct {
	cfg  Config
	http *http.Client

	// unusable is the reason this client can never connect — a CA bundle it
	// was told to verify against and could not read. It is kept rather than
	// returned from New because a client that cannot verify must fail every
	// query loudly, and the alternatives are both worse: returning an error
	// from New would have every one of its forty call sites decide what to do
	// about it, and carrying on without the bundle would verify against the
	// host's roots, which for a store whose certificate came from the
	// platform's own CA is a connection that fails for a reason nobody can
	// read.
	unusable error
}

// New builds a client with a timeout that keeps a wedged store from stalling
// a reconcile.
//
// A https connection is verified — hostname and chain, against cfg.CAFile
// where there is one and the host's roots where there is not. There is no
// switch here that turns that off, and no path that falls back to plaintext:
// a store the chart says speaks TLS is either reached over verified TLS or
// not reached at all.
func New(cfg Config) *Client {
	client := &Client{cfg: cfg, http: &http.Client{Timeout: 15 * time.Second}}
	if cfg.scheme() != SchemeHTTPS || cfg.CAFile == "" {
		return client
	}

	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		client.unusable = fmt.Errorf("reading the telemetry store's CA bundle %s: %w", cfg.CAFile, err)
		return client
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		client.unusable = fmt.Errorf("the telemetry store's CA bundle %s holds no certificate", cfg.CAFile)
		return client
	}
	client.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	}}
	return client
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
	return c.QueryWithParams(ctx, query, nil)
}

// QueryError is a statement ClickHouse refused as written — a syntax error, an
// unknown column — as opposed to a store that could not be reached. It exists
// so a caller-authored query can be answered with "fix your query" rather than
// "the platform is broken".
type QueryError struct {
	Status  string
	Message string
}

func (e *QueryError) Error() string {
	return fmt.Sprintf("clickhouse refused the query (%s): %s", e.Status, e.Message)
}

// QueryWithParams runs a statement whose `{name:Type}` placeholders are filled
// in by ClickHouse itself. Anything that reaches a query from a request — a
// project name, a search term, a row limit — goes through here rather than
// into the query text.
func (c *Client) QueryWithParams(ctx context.Context, query string, params map[string]string) (string, error) {
	return c.queryWithSettings(ctx, query, params, nil)
}

// readonlySettings are applied to every query that carries caller-written
// query text. readonly=2 forbids writes and DDL while still allowing the
// request's own parameters and limits; the execution cap keeps an expensive
// expression from holding a connection open until the client times out anyway.
var readonlySettings = map[string]string{
	"readonly":           "2",
	"max_execution_time": "10",
}

// queryWithSettings is QueryWithParams plus per-query ClickHouse settings.
func (c *Client) queryWithSettings(ctx context.Context, query string, params, settings map[string]string) (string, error) {
	values := url.Values{"database": {c.cfg.Database}}
	for name, value := range params {
		values.Set("param_"+name, value)
	}
	for name, value := range settings {
		values.Set(name, value)
	}
	return c.do(ctx, query, values)
}

// execOutsideDatabase runs a statement without naming a database to run it in.
//
// There is exactly one such statement, and it is the CREATE DATABASE itself:
// ClickHouse resolves the `database` parameter before it looks at the query, so
// a CREATE DATABASE sent with it is refused with "Database kitchen does not
// exist" — which is the very thing it was about to fix. On the chart's own
// ClickHouse this never showed, because the StatefulSet creates the database
// from CLICKHOUSE_DB before the operator ever connects; against an external
// store it is the difference between the platform bringing its own schema up
// and an install that cannot start.
func (c *Client) execOutsideDatabase(ctx context.Context, query string) error {
	_, err := c.do(ctx, query, url.Values{})
	return err
}

// do posts one statement and reads its answer.
func (c *Client) do(ctx context.Context, query string, values url.Values) (string, error) {
	if c.unusable != nil {
		return "", c.unusable
	}
	endpoint := c.cfg.endpoint() + "?" + values.Encode()

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

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// ClickHouse reports errors as the body of a non-2xx response; it
		// is the only useful diagnostic, so a bounded prefix of it goes
		// into the error.
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		if err != nil {
			return "", err
		}
		// A 4xx is ClickHouse judging the statement, not failing at it.
		if resp.StatusCode < 500 {
			return "", &QueryError{Status: resp.Status, Message: strings.TrimSpace(string(body))}
		}
		return "", fmt.Errorf("clickhouse returned %s: %s",
			resp.Status, strings.TrimSpace(string(body)))
	}

	// Answers are read whole, so the size of one is the operator's memory:
	// bounded, and loudly rather than by handing back a truncated answer as
	// if it were the whole thing.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxResponseBytes {
		return "", fmt.Errorf("clickhouse returned more than %d bytes; ask for less at a time", maxResponseBytes)
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
