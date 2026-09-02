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

package provider

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Bermos/Kitchen/internal/provider/cache"
)

// The two cache providers' probes. They answer the same three-part question
// every other probe answers — is the provider reachable, and does the
// credential work — and they answer it very differently, because one of them
// is this cluster.

// ValkeyProbe answers for the provider that is not somewhere else: the
// platform writes a Valkey per claim into the cluster it was installed in,
// with the operator's own account.
//
// There is nothing to reach and no credential to check, and saying so is the
// honest answer rather than a hollow one. What could fail here fails per
// claim — a cluster with no default StorageClass cannot give a *queue* its
// volume — and it fails on the claim, where the message can name the claim
// that asked.
type ValkeyProbe struct{}

// Probe reports that the platform provisions caches itself.
func (p *ValkeyProbe) Probe(context.Context) Result {
	return accepted("the platform runs a Valkey per claim in this cluster, with the operator's own account; " +
		"this connection holds no credential")
}

// RedisProbe answers for a server somebody else runs, by speaking enough of
// the protocol to be sure: it opens the connection, authenticates if the URL
// carries a password, and asks the server to PING.
//
// It talks RESP by hand rather than taking a client library for it. Three
// commands and two replies is not worth a dependency, and the whole of what
// this has to establish is that something on the other end answers and that
// the credential it was given is accepted.
type RedisProbe struct {
	// URL of the server, as the Connection's credential holds it.
	URL string
	// Timeout bounds the whole exchange. Zero takes a sensible default.
	Timeout time.Duration
}

// Probe opens the connection and asks the server who it is.
func (p *RedisProbe) Probe(ctx context.Context) Result {
	parsed, err := url.Parse(strings.TrimSpace(p.URL))
	if err != nil {
		return unreachableBecause("this connection's url is not a URL: " + err.Error())
	}
	if parsed.Host == "" {
		return unreachableBecause("this connection's url names no host")
	}

	timeout := p.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	address := parsed.Host
	if parsed.Port() == "" {
		address = net.JoinHostPort(parsed.Hostname(), "6379")
	}

	conn, err := dialRedis(ctx, parsed.Scheme, address)
	if err != nil {
		return unreachable(err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	reader := bufio.NewReader(conn)

	// AUTH first where there is a credential, so that a wrong one is
	// reported as a refused credential rather than as a refused PING.
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		if password != "" {
			user := parsed.User.Username()
			command := fmt.Sprintf("AUTH %s\r\n", password)
			if user != "" {
				command = fmt.Sprintf("AUTH %s %s\r\n", user, password)
			}
			if _, err := conn.Write([]byte(command)); err != nil {
				return unreachable(err)
			}
			line, err := reader.ReadString('\n')
			if err != nil {
				return unreachable(err)
			}
			if !strings.HasPrefix(line, "+OK") {
				return Result{
					Reachable:         true,
					CredentialChecked: true,
					Message:           "the server refused the credential: " + strings.TrimSpace(line),
				}
			}
		}
	}

	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		return unreachable(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return unreachable(err)
	}
	switch {
	case strings.HasPrefix(line, "+PONG"):
		return accepted("the server answered PING")
	case strings.HasPrefix(line, "-NOAUTH"), strings.HasPrefix(line, "-WRONGPASS"):
		// The URL carried no password and the server wants one.
		return Result{
			Reachable:         true,
			CredentialChecked: true,
			Message: "the server requires a password and this connection's url carries none: " +
				strings.TrimSpace(line),
		}
	default:
		return Result{
			Reachable: true,
			Message:   "the server answered something other than PONG: " + strings.TrimSpace(line),
		}
	}
}

// dialRedis opens the connection, over TLS for a rediss:// URL.
func dialRedis(ctx context.Context, scheme, address string) (net.Conn, error) {
	dialer := &net.Dialer{}
	if scheme == "rediss" {
		return (&tls.Dialer{NetDialer: dialer}).DialContext(ctx, "tcp", address)
	}
	return dialer.DialContext(ctx, "tcp", address)
}

// newRedisProbe builds the probe from a Connection's credential.
func newRedisProbe(creds *corev1.Secret) (Probe, error) {
	if creds == nil {
		return nil, fmt.Errorf("a %s connection needs a credential holding the server's %s",
			cache.ProviderRedis, cache.CredentialKeyURL)
	}
	serverURL := strings.TrimSpace(string(creds.Data[cache.CredentialKeyURL]))
	if serverURL == "" {
		return nil, fmt.Errorf("credentials secret %q has no %q key", creds.Name, cache.CredentialKeyURL)
	}
	return &RedisProbe{URL: serverURL}, nil
}
