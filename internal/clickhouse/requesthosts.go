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
	"fmt"
	"strings"
)

// Dropping a set of hostnames from a request read.
//
// A request row is attributed by the authority the client asked for, and the
// authorities an environment answers on are not all of them served by the
// environment: a hostname can be attached to it and not yet routed, and the
// platform's edge answers those requests itself. The store has no opinion
// about which hostnames those are — the caller names them, exactly as it names
// the health check it wants left out — and this is only the predicate that
// leaves them behind.
//
// `host` is in the ordering key of both rollups and is LowCardinality on the
// raw table, so the exclusion costs a key-range skip rather than a scan.

// hostExclusion renders the predicate that drops a set of hostnames,
// registering the parameters it names.
//
// It answers an empty string when there is nothing to exclude, so callers
// append nothing rather than a tautology. prefix is how the read spells its
// columns — the rollup reads alias their table `r`, the raw listing does not
// alias at all — and every hostname travels as a query parameter, because it
// comes from a Domain somebody attached.
func hostExclusion(hosts []string, prefix string, params map[string]string) string {
	names := make([]string, 0, len(hosts))
	seen := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSpace(host))
		// An empty hostname is the unattributed bucket's own value, and
		// excluding it would drop the traffic nobody could attribute from a
		// read that never contained any of it.
		if host == "" {
			continue
		}
		if _, done := seen[host]; done {
			continue
		}
		seen[host] = struct{}{}

		name := fmt.Sprintf("excludedHost%d", len(names))
		params[name] = host
		names = append(names, fmt.Sprintf("{%s:String}", name))
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("%shost NOT IN (%s)", prefix, strings.Join(names, ", "))
}
