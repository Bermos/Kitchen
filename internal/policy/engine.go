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

package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/storage/inmem"
)

// The verdicts. Exactly three, and the middle one exists so that an emergency
// deployment is never a lie: rules still fire, the exception that waived them
// is named, and the verdict says both things at once.
const (
	// VerdictAllowed: no rule fired.
	VerdictAllowed = "allowed"
	// VerdictAllowedWithException: every rule that fired was waived by an
	// active exception.
	VerdictAllowedWithException = "allowed-with-exception"
	// VerdictBlocked: at least one fired rule stands unwaived.
	VerdictBlocked = "blocked"
)

// denyQuery is the bundle contract: a bundle implements
// data.kitchen.promotion.deny as a set of {"rule": id, "message": why}
// objects, and implements nothing else this engine reads.
const denyQuery = "data.kitchen.promotion.deny"

// FiredRule is one rule that fired, with whether an exception waived it. The
// rule fires either way — waiving changes the verdict, never the facts.
type FiredRule struct {
	Rule    string `json:"rule"`
	Message string `json:"message"`
	Waived  bool   `json:"waived,omitempty"`
	// Exception names the exception that waived the rule, so a reader of the
	// decision can go and read why.
	Exception string `json:"exception,omitempty"`
}

// Result is one evaluation's outcome: the verdict, and every rule that fired.
type Result struct {
	Verdict string      `json:"verdict"`
	Fired   []FiredRule `json:"fired"`
}

// Evaluate asks one bundle one question. It is the single code path every
// verdict on this platform comes through — promotion, rescan, replay and the
// eligibility preview all call this — and it is a pure function of its two
// arguments: the bundle is compiled with no network builtins (http.send,
// net.*, opa.runtime are removed from the capability set, so a bundle using
// them fails compilation), and the input is everything the rules can see.
//
// Exceptions carried in the input are applied here, against the input's own
// clock (Input.At), which is what makes a replay reproduce the original
// waiving however much later it runs.
func Evaluate(ctx context.Context, bundle Bundle, input Input) (Result, error) {
	if len(bundle) == 0 {
		return Result{}, fmt.Errorf("the policy bundle is empty: nothing to evaluate against")
	}

	// The input reaches the engine through its canonical encoding, so what is
	// evaluated is byte-for-byte what Input.Digest identifies.
	canonical, err := input.Canonical()
	if err != nil {
		return Result{}, fmt.Errorf("the policy input could not be encoded: %w", err)
	}
	var document any
	if err := json.Unmarshal(canonical, &document); err != nil {
		return Result{}, fmt.Errorf("the policy input could not be decoded: %w", err)
	}

	options := []func(*rego.Rego){
		rego.Query(denyQuery),
		rego.Input(document),
		rego.Capabilities(evaluationCapabilities()),
	}

	paths := make([]string, 0, len(bundle))
	for path := range bundle {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	modules, data := 0, ""
	for _, path := range paths {
		switch {
		case strings.HasSuffix(path, ".rego"):
			options = append(options, rego.Module(path, bundle[path]))
			modules++
		case path == "data.json" || strings.HasSuffix(path, "/data.json"):
			if data != "" {
				return Result{}, fmt.Errorf(
					"the bundle carries more than one data.json (%s and %s); a bundle has at most one", data, path)
			}
			data = path
			var static map[string]any
			if err := json.Unmarshal([]byte(bundle[path]), &static); err != nil {
				return Result{}, fmt.Errorf("the bundle's %s is not a JSON object: %w", path, err)
			}
			options = append(options, rego.Store(inmem.NewFromObject(static)))
		default:
			return Result{}, fmt.Errorf(
				"the bundle carries %q, which is neither a .rego module nor a data.json", path)
		}
	}
	if modules == 0 {
		return Result{}, fmt.Errorf("the policy bundle holds no .rego modules")
	}

	results, err := rego.New(options...).Eval(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("the policy bundle could not be evaluated: %w", err)
	}
	if len(results) == 0 {
		// The query was undefined: the bundle never defines the entry point.
		// That is a broken bundle, not an allowed artifact.
		return Result{}, fmt.Errorf(
			"the policy bundle does not define %s, which is the entry point every bundle implements", denyQuery)
	}

	fired, err := firedFrom(results)
	if err != nil {
		return Result{}, err
	}
	fired = ApplyExceptions(fired, input.Exceptions, input.At)
	return Result{Verdict: verdictOf(fired), Fired: fired}, nil
}

// firedFrom reads the deny set out of the evaluation. An entry that does not
// carry a rule id is refused rather than smoothed over: a requirement, an
// exception and a stored decision all address rules by id, and an anonymous
// violation would be one nothing can name.
func firedFrom(results rego.ResultSet) ([]FiredRule, error) {
	fired := []FiredRule{}
	for _, result := range results {
		for _, expression := range result.Expressions {
			entries, ok := expression.Value.([]any)
			if !ok {
				return nil, fmt.Errorf("%s did not evaluate to a set of objects", denyQuery)
			}
			for _, raw := range entries {
				entry, ok := raw.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("%s produced an entry that is not an object: %v", denyQuery, raw)
				}
				rule, _ := entry["rule"].(string)
				if rule == "" {
					return nil, fmt.Errorf(
						"%s produced an entry without a rule id; every deny entry is {\"rule\": ..., \"message\": ...}",
						denyQuery)
				}
				message, _ := entry["message"].(string)
				fired = append(fired, FiredRule{Rule: rule, Message: message})
			}
		}
	}
	// Deterministic order, so the same evaluation stores the same bytes.
	sort.Slice(fired, func(i, j int) bool {
		if fired[i].Rule != fired[j].Rule {
			return fired[i].Rule < fired[j].Rule
		}
		return fired[i].Message < fired[j].Message
	})
	return fired, nil
}

// ApplyExceptions marks fired rules waived where an exception still active at
// `at` lists the rule's id. Rules are never removed: an exception changes the
// verdict, and the record keeps saying what fired and who waived it.
//
// Activity is judged against `at` — the evaluation's own clock — rather than
// against whoever is reading: a decision made under an exception replays with
// the same waiving after the exception has expired, because the question is
// what was true when it was decided.
func ApplyExceptions(fired []FiredRule, exceptions []Exception, at time.Time) []FiredRule {
	out := make([]FiredRule, len(fired))
	copy(out, fired)
	for i := range out {
		for _, exception := range exceptions {
			if exception.ExpiresAt.IsZero() || !at.Before(exception.ExpiresAt) {
				continue
			}
			for _, rule := range exception.RuleIDs {
				if rule == out[i].Rule {
					out[i].Waived = true
					out[i].Exception = exception.Name
					break
				}
			}
			if out[i].Waived {
				break
			}
		}
	}
	return out
}

// verdictOf reduces the fired rules to the one word a promotion acts on.
func verdictOf(fired []FiredRule) string {
	if len(fired) == 0 {
		return VerdictAllowed
	}
	for _, rule := range fired {
		if !rule.Waived {
			return VerdictBlocked
		}
	}
	return VerdictAllowedWithException
}

// evaluationCapabilities is OPA's own capability set for this version with
// every road out of the sandbox removed: http.send, the net.* builtins and
// opa.runtime. A bundle that names any of them fails to compile, which is the
// property that makes a stored decision replayable — a policy that fetched
// during evaluation could not be re-run against a historical input and mean
// anything.
func evaluationCapabilities() *ast.Capabilities {
	capabilities := ast.CapabilitiesForThisVersion()
	kept := make([]*ast.Builtin, 0, len(capabilities.Builtins))
	for _, builtin := range capabilities.Builtins {
		if forbiddenBuiltin(builtin.Name) {
			continue
		}
		kept = append(kept, builtin)
	}
	capabilities.Builtins = kept
	// Belt and braces: even if a network builtin slipped back in, an empty
	// (non-nil) allow list permits it to reach nothing.
	capabilities.AllowNet = []string{}
	return capabilities
}

func forbiddenBuiltin(name string) bool {
	return name == "http.send" ||
		strings.HasPrefix(name, "net.") ||
		strings.HasPrefix(name, "opa.runtime")
}
