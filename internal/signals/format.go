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

package signals

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// How findings say numbers.
//
// A finding is read by a person deciding whether to get out of bed, so the
// numbers are rendered the way that person thinks: 96% not 0.9612, 492Mi not
// 515899392, 12m not 720.0000001s. Rendering happens here rather than at each
// call site so that two rules cannot describe the same quantity differently —
// and so that the strings the tests assert on are worth asserting on.

// percent renders a 0..1 fraction the way an operator quotes it.
func percent(fraction float64) string {
	return fmt.Sprintf("%.0f%%", fraction*100)
}

// binaryUnits are Kubernetes' own, because the limit the number is being
// compared against was written in them.
var binaryUnits = []string{"Ki", "Mi", "Gi", "Ti", "Pi"}

// bytes renders a byte count in the units a resource limit is written in.
func bytes(count float64) string {
	if count < 1024 {
		return fmt.Sprintf("%.0fB", count)
	}
	value := count
	unit := ""
	for _, candidate := range binaryUnits {
		value /= 1024
		unit = candidate
		if value < 1024 {
			break
		}
	}
	if value < 10 {
		return fmt.Sprintf("%.1f%s", value, unit)
	}
	return fmt.Sprintf("%.0f%s", value, unit)
}

// duration renders a span at the resolution a person would say it out loud: a
// couple of significant units and no more.
func duration(span time.Duration) string {
	if span < time.Minute {
		return fmt.Sprintf("%.0fs", math.Max(span.Seconds(), 0))
	}
	if span < time.Hour {
		return fmt.Sprintf("%dm", int(span.Minutes()))
	}
	hours := int(span.Hours())
	minutes := int(span.Minutes()) % 60
	if span < 24*time.Hour {
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	days := hours / 24
	if remainder := hours % 24; remainder != 0 {
		return fmt.Sprintf("%dd%dh", days, remainder)
	}
	return fmt.Sprintf("%dd", days)
}

// milliseconds renders a latency without pretending to sub-millisecond
// precision the edge does not have.
func milliseconds(value float64) string {
	if value >= 1000 {
		return fmt.Sprintf("%.1fs", value/1000)
	}
	return fmt.Sprintf("%.0fms", value)
}

// plural is the difference between "1 restarts" and a sentence somebody wrote.
func plural(count int, singular, many string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, singular)
	}
	return fmt.Sprintf("%d %s", count, many)
}

// sentence joins the clauses of a detail. The first clause is the headline
// number — the environment page's diagnostics strip renders it in parentheses
// after the title — so callers put it first and everything explanatory after.
func sentence(clauses ...string) string {
	kept := make([]string, 0, len(clauses))
	for _, clause := range clauses {
		if clause = strings.TrimSpace(clause); clause != "" {
			kept = append(kept, clause)
		}
	}
	return strings.Join(kept, "; ")
}
