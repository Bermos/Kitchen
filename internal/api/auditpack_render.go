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

package api

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The human rendering.
//
// The audience for an audit pack is frequently not an engineer, and a person
// who has been handed a hundred kilobytes of JSON has not been handed
// evidence — they have been handed a task. So the same document is served as
// one self-contained HTML page: no stylesheet to fetch, no script, no font,
// nothing that needs a network. It prints.
//
// # Why the server renders it and not the dashboard
//
// Because the pack is a document that *leaves*. It is emailed, printed, put
// in a data room, and read by somebody who will never have a login on this
// platform. A rendering that required the dashboard to be running and the
// reader to be signed in would not be a document that left; it would be a
// screen. The dashboard shows the summary and hands over both files, which is
// the right division: the screen is where you take a pack, the page is what
// you take.
//
// # It is not signed, and it says so
//
// The rendering is derived from the pack and from nothing else, so it is
// deterministic — but what a signature covers has to be one unambiguous
// sequence of bytes, and "the same information, laid out for a person" is not
// that. So the page carries the pack's digest at the top and points at the
// two files the signature is actually about. A printout that cannot be tied
// back to bytes is decoration.
//
// # What it leaves out
//
// The decisions' full canonical inputs, and the signed envelopes. Both are
// megabytes of base64 and neither is legible; the page names them, counts
// them and says where they are. Everything a person reads — who approved
// what, which waivers were open, what is running that no longer meets its bar
// — is here in full.

// writeAuditPackHTML renders the pack for a reader.
func (s *Server) writeAuditPackHTML(
	w http.ResponseWriter, project *kitchenv1alpha1.Project, pack auditPack, digest string, size int,
) {
	page := &bytes.Buffer{}
	if err := auditPackTemplate.Execute(page, packRender{
		Pack:   pack,
		Digest: digest,
		Bytes:  size,
	}); err != nil {
		s.log().Error(err, "the audit pack could not be rendered", "project", project.Name)
		writeJSON(w, http.StatusInternalServerError, errorBody{
			Error: "the pack was assembled but could not be rendered; ?format=json serves it whole",
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Kitchen-Pack-Digest", digest)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("inline; filename=%q", packFilename(pack, "html")))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page.Bytes())
}

// packRender is what the template is handed: the document, and the two facts
// about it that are not inside it.
type packRender struct {
	Pack   auditPack
	Digest string
	Bytes  int
}

// packFuncs are the four things the template cannot do for itself.
var packFuncs = template.FuncMap{
	// stamp is the one date format the page uses. UTC, always, because a
	// document read in three time zones must not read differently in each.
	"stamp": func(at time.Time) string {
		if at.IsZero() {
			return "—"
		}
		return at.UTC().Format("2006-01-02 15:04 UTC")
	},
	"maybe": func(at *time.Time) string {
		if at == nil {
			return "—"
		}
		return at.UTC().Format("2006-01-02 15:04 UTC")
	},
	"join": func(values []string) string {
		if len(values) == 0 {
			return "—"
		}
		return strings.Join(values, ", ")
	},
	"orDash": func(value string) string {
		if strings.TrimSpace(value) == "" {
			return "—"
		}
		return value
	},
	"yesNo": func(value bool) string {
		if value {
			return "yes"
		}
		return "no"
	},
	"short": func(value string) string {
		if len(value) <= 19 {
			return value
		}
		return value[:19] + "…"
	},
}

// auditPackTemplate is the page. It is parsed once, at start-up, so a
// malformed template is a panic on the first request rather than a 500 on the
// one somebody is waiting for.
var auditPackTemplate = template.Must(template.New("audit-pack").Funcs(packFuncs).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Audit pack — {{.Pack.Project}} — {{.Pack.Range.From}} to {{.Pack.Range.To}}</title>
<style>
  :root { color-scheme: light; }
  body { font: 14px/1.55 ui-sans-serif, system-ui, -apple-system, "Segoe UI", Helvetica, Arial, sans-serif;
         color: #16181d; background: #fff; margin: 0; padding: 2rem 1.5rem 6rem; }
  main { max-width: 60rem; margin: 0 auto; }
  h1 { font-size: 1.5rem; margin: 0 0 .25rem; }
  h2 { font-size: 1.05rem; margin: 2.5rem 0 .5rem; padding-bottom: .3rem; border-bottom: 2px solid #16181d; }
  h3 { font-size: .95rem; margin: 1.5rem 0 .4rem; }
  p  { margin: .5rem 0; }
  .lede { color: #4a4f5a; margin: 0 0 1.5rem; }
  .note { color: #4a4f5a; font-size: 12.5px; margin: .4rem 0 1rem; }
  .warn { background: #fff6e5; border-left: 3px solid #b8791a; padding: .6rem .8rem; margin: .8rem 0; }
  .bad  { background: #fdecec; border-left: 3px solid #b83535; padding: .6rem .8rem; margin: .8rem 0; }
  table { border-collapse: collapse; width: 100%; margin: .5rem 0 1rem; font-size: 12.5px; }
  th, td { text-align: left; vertical-align: top; padding: .35rem .5rem; border-bottom: 1px solid #e3e5ea; }
  th { color: #4a4f5a; font-weight: 600; white-space: nowrap; }
  code, .mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; }
  pre { background: #f5f6f8; padding: .8rem; overflow-x: auto; font-size: 11.5px; border-radius: 4px; }
  dl.facts { display: grid; grid-template-columns: 12rem 1fr; gap: .2rem .8rem; margin: .5rem 0 1rem; font-size: 13px; }
  dl.facts dt { color: #4a4f5a; }
  dl.facts dd { margin: 0; }
  .empty { color: #6b7280; font-style: italic; }
  @media print { body { padding: 0; } h2 { page-break-after: avoid; } table { page-break-inside: auto; } }
</style>
</head><body><main>

<h1>Audit pack — {{.Pack.Project}}</h1>
<p class="lede">{{.Pack.Range.From}} → {{.Pack.Range.To}} ({{.Pack.Range.HalfOpen}})</p>

<dl class="facts">
  <dt>Pack digest</dt><dd class="mono">{{.Digest}}</dd>
  <dt>Pack size</dt><dd>{{.Bytes}} bytes</dd>
  <dt>Schema</dt><dd class="mono">{{.Pack.Schema}}</dd>
  <dt>Produced by</dt><dd>{{.Pack.Platform.Name}} {{orDash .Pack.Platform.Version}}{{if .Pack.Platform.ClusterName}} · {{.Pack.Platform.ClusterName}}{{end}}</dd>
  <dt>Signed</dt><dd>{{yesNo .Pack.Verification.Signed}}{{if .Pack.Verification.KeyID}} · key {{.Pack.Verification.KeyID}}{{end}}</dd>
</dl>

<p class="note">This page is a rendering. It is <strong>not</strong> the signed document: the signature is
over the machine-readable pack, whose sha256 is printed above. Fetch that pack and its signature with
<code>?format=json</code> and <code>?format=dsse</code> on the same address, and check them by the
procedure below.</p>

{{if not .Pack.Verification.Signed}}<div class="bad"><strong>Unsigned.</strong> {{.Pack.Verification.Message}}</div>{{end}}
{{if .Pack.Retention.Truncated}}<div class="warn"><strong>Retention has truncated this window.</strong> {{.Pack.Retention.Message}}</div>{{end}}
{{if not .Pack.Platform.AuditRecording}}<div class="warn"><strong>The audit log is not recording.</strong> {{orDash .Pack.Platform.AuditMessage}}</div>{{end}}
{{if not .Pack.Platform.Rescanning}}<div class="warn"><strong>Continuous re-evaluation is not running.</strong> {{orDash .Pack.Platform.RescanMessage}} Nothing in the drift section is a statement about today.</div>{{end}}

<h2>1 · How to check this pack without Kitchen</h2>
<p class="note">{{.Pack.Verification.Warning}}</p>
<ol>{{range .Pack.Verification.Procedure}}<li class="mono">{{.}}</li>{{end}}</ol>
{{if .Pack.Verification.PublicKey}}<pre>{{.Pack.Verification.PublicKey}}</pre>{{end}}

<h2>2 · What this pack can and cannot answer for</h2>
<p>{{.Pack.Reproducibility.Claim}}</p>
<dl class="facts">
  <dt>Fixed by the range</dt><dd>{{join .Pack.Reproducibility.RangeBound}}</dd>
  <dt>The estate as it is now</dt><dd>{{join .Pack.Reproducibility.CurrentState}}</dd>
  <dt>Audit retention</dt><dd>{{.Pack.Retention.AuditDays}} days (floor {{.Pack.Retention.FloorDays}}{{if .Pack.Retention.Overridden}}, <strong>overridden</strong>{{end}}) · oldest record {{maybe .Pack.Retention.Oldest}}</dd>
  <dt>Coverage</dt><dd>{{.Pack.Retention.Message}}</dd>
</dl>
{{if .Pack.Retention.Overridden}}<div class="warn"><strong>The audit retention is below the documented floor.</strong>
Reason: {{orDash .Pack.Retention.OverrideReason}} — approved by {{orDash .Pack.Retention.OverrideApprovedBy}}.</div>{{end}}
{{with .Pack.Platform.ClockSync}}
<p class="note"><strong>Clocks.</strong> {{orDash .Method}} · {{.Nodes}} nodes measured, {{.Drifted}} beyond {{.MaxDriftSeconds}}s
· worst {{orDash .WorstNode}} at {{.WorstDriftMillis}} ms · checked {{maybe .Checked}}. {{.Message}}</p>
{{end}}
<p class="note">{{.Pack.Retention.Note}}</p>

<h2>3 · Inventory</h2>
<dl class="facts">
  <dt>Project</dt><dd>{{.Pack.Inventory.Project.Name}} · created {{stamp .Pack.Inventory.Project.CreatedAt}}</dd>
  <dt>Data class</dt><dd>{{.Pack.Inventory.Project.DataClass}}</dd>
  <dt>Criticality</dt><dd>{{.Pack.Inventory.Project.Criticality}}{{if .Pack.Inventory.Project.RTO}} · RTO {{.Pack.Inventory.Project.RTO}}{{end}}{{if .Pack.Inventory.Project.RPO}} · RPO {{.Pack.Inventory.Project.RPO}}{{end}}</dd>
  <dt>Repository</dt><dd class="mono">{{orDash .Pack.Inventory.Project.Repository}} ({{orDash .Pack.Inventory.Project.Branch}})</dd>
  <dt>Reviewed pull request required</dt><dd>{{yesNo .Pack.Inventory.Project.RequirePullRequest}}</dd>
  <dt>Third parties</dt><dd>{{join .Pack.Inventory.ThirdParties}}</dd>
</dl>

<h3>Environments</h3>
<table><thead><tr><th>Name</th><th>Type</th><th>Class</th><th>Residency</th><th>Criticality</th><th>Release</th><th>Bar</th><th>Owners</th></tr></thead><tbody>
{{range .Pack.Inventory.Environments}}<tr>
  <td>{{.Name}}</td><td>{{.Type}}</td><td>{{.DataClass}}</td><td>{{.Residency}}</td>
  <td>{{.Criticality}}{{if .RTO}} · {{.RTO}}{{end}}{{if .Inherited}} <span class="empty">(inherited: {{join .Inherited}})</span>{{end}}</td>
  <td>{{orDash .Release}}</td>
  <td class="mono">{{if .BundleDigest}}{{short .BundleDigest}}{{else}}<span class="empty">declares none</span>{{end}}</td>
  <td>{{if .Owners}}{{join .Owners}}{{else}}<span class="empty">operators only</span>{{end}}</td>
</tr>{{else}}<tr><td colspan="8" class="empty">no environments</td></tr>{{end}}
</tbody></table>

<h3>Resource claims</h3>
<table><thead><tr><th>Name</th><th>Type</th><th>Third party</th><th>Class</th><th>Data derives from</th><th>Location</th><th>Phase</th></tr></thead><tbody>
{{range .Pack.Inventory.Claims}}<tr>
  <td>{{.Name}}</td><td>{{.Type}}</td><td>{{orDash .Provider}}</td><td>{{.DataClass}}</td>
  <td>{{.Provenance}}</td><td>{{.Residency}}</td><td>{{orDash .Phase}}</td>
</tr>{{else}}<tr><td colspan="7" class="empty">no resource claims</td></tr>{{end}}
</tbody></table>

<h3>Connections</h3>
<table><thead><tr><th>Name</th><th>Third party</th><th>Used for</th><th>Capabilities</th></tr></thead><tbody>
{{range .Pack.Inventory.Connections}}<tr>
  <td>{{.Name}}</td><td>{{orDash .Provider}}</td><td>{{join .UsedFor}}</td><td>{{join .Capabilities}}</td>
</tr>{{else}}<tr><td colspan="4" class="empty">no connections</td></tr>{{end}}
</tbody></table>
<p class="note">The credential behind each connection is held by the platform and is never in this document —
the API does not read a credential back and an export is not an exception.</p>

<h3>Domains</h3>
<table><thead><tr><th>Hostname</th><th>Environment</th><th>Verified</th><th>TLS</th></tr></thead><tbody>
{{range .Pack.Inventory.Domains}}<tr><td class="mono">{{.Hostname}}</td><td>{{.Environment}}</td><td>{{yesNo .Verified}}</td><td>{{orDash .TLSMode}}</td></tr>
{{else}}<tr><td colspan="4" class="empty">no custom domains</td></tr>{{end}}
</tbody></table>

<h3>Releases in this window</h3>
<p class="note">{{.Pack.Inventory.Scope.Releases}}.</p>
<table><thead><tr><th>Release</th><th>Cut</th><th>In range</th><th>Image</th></tr></thead><tbody>
{{range .Pack.Inventory.Releases}}<tr><td>{{.Name}}</td><td>{{stamp .CreatedAt}}</td><td>{{yesNo .InRange}}</td><td class="mono">{{orDash .Image}}</td></tr>
{{else}}<tr><td colspan="4" class="empty">no releases</td></tr>{{end}}
</tbody></table>

<h2>4 · Who holds what, and who last checked</h2>
<table><thead><tr><th>Subject</th><th>Address</th><th>Role</th></tr></thead><tbody>
{{range .Pack.Access.Grants}}<tr><td class="mono">{{.Subject}}</td><td>{{orDash .Email}}</td><td>{{.Role}}</td></tr>
{{else}}<tr><td colspan="3" class="empty">no grants on this project — the platform's operators only</td></tr>{{end}}
</tbody></table>
<p class="note">{{.Pack.Access.Note}}</p>

{{range .Pack.Access.Cycles}}
<h3>Recertification {{.Name}} — {{.Phase}}</h3>
<dl class="facts">
  <dt>Scope</dt><dd>{{.Scope}}{{if .Project}} · {{.Project}}{{end}}</dd>
  <dt>Opened / closed</dt><dd>{{maybe .OpenedAt}} by {{orDash .OpenedBy}} → {{maybe .ClosedAt}}{{if .ClosedBy}} by {{.ClosedBy}}{{end}}</dd>
  <dt>Reviewers</dt><dd>{{join .Reviewers}}</dd>
  <dt>Tally</dt><dd>{{.Confirmed}} confirmed · {{.Revoked}} revoked · {{.Pending}} pending · {{.SelfReviewed}} self-reviewed · {{.Orphaned}} orphaned (of {{.EntriesTotal}})</dd>
  <dt>Signed artefact</dt><dd>{{if .RecordID}}<span class="mono">{{.RecordID}}</span> · subject <span class="mono">{{short .Subject}}</span> · {{maybe .SignedAt}}{{else}}<span class="empty">none — {{orDash .ArtifactNote}}</span>{{end}}</dd>
</dl>
<table><thead><tr><th>Subject</th><th>Grant</th><th>Role</th><th>Decision</th><th>By</th><th>When</th></tr></thead><tbody>
{{range .Entries}}<tr>
  <td class="mono">{{.Subject}}{{if .Email}} <span class="empty">{{.Email}}</span>{{end}}</td>
  <td>{{.Grant}}</td><td>{{.Role}}</td>
  <td>{{.Decision}}{{if .SelfReview}} <strong>(self-review)</strong>{{end}}{{if .Orphaned}} <strong>(orphaned)</strong>{{end}}</td>
  <td>{{orDash .DecidedBy}}</td><td>{{maybe .DecidedAt}}</td>
</tr>{{else}}<tr><td colspan="6" class="empty">no grants on this project were in this cycle</td></tr>{{end}}
</tbody></table>
{{if .EntriesNote}}<p class="note">{{.EntriesNote}}</p>{{end}}
{{else}}
<p class="empty">No recertification cycle covered this project inside the window.</p>
{{end}}

<h2>5 · Change log</h2>
<table><thead><tr><th>Release</th><th>Cut</th><th>Commit</th><th>Author</th><th>Approved by</th><th>Independent</th></tr></thead><tbody>
{{range .Pack.ChangeLog}}<tr>
  <td>{{.Release}}</td><td>{{stamp .CreatedAt}}</td>
  <td class="mono">{{short (orDash .Commit)}}<br><span class="empty">{{orDash .Message}}</span></td>
  <td>{{orDash .Author}}</td>
  {{with .Review}}<td>{{if .Approvers}}{{join .Approvers}}{{else}}<span class="empty">nobody</span>{{end}}{{if .Exception}}<br><strong>waived by {{.Exception}}</strong>{{end}}{{if .MachineIdentity}}<br><strong>machine identity {{.MachineIdentity}}</strong>{{end}}</td>
  <td>{{if .Independent}}yes{{else if .SelfApproved}}<strong>self-approved</strong>{{else if .Required}}<strong>no</strong>{{else}}<span class="empty">not required</span>{{end}}</td>
  {{else}}<td colspan="2" class="empty">{{orDash .ReviewNote}}</td>{{end}}
</tr>{{else}}<tr><td colspan="6" class="empty">no releases in this window</td></tr>{{end}}
</tbody></table>

<h2>6 · Promotions and the decisions behind them</h2>
<table><thead><tr><th>When</th><th>Environment</th><th>Release</th><th>Asked by</th><th>Verdict</th><th>Unmet rules</th><th>Decision</th></tr></thead><tbody>
{{range .Pack.Promotions}}<tr>
  <td>{{stamp .CreatedAt}}</td><td>{{.Environment}}</td><td>{{.Release}}</td>
  <td>{{orDash .RequestedBy}} <span class="empty">({{.Trigger}})</span></td>
  <td>{{orDash .Verdict}}</td><td>{{join .UnmetRules}}</td>
  <td class="mono">{{short (orDash .DecisionID)}}</td>
</tr>{{else}}<tr><td colspan="7" class="empty">nothing was promoted in this window</td></tr>{{end}}
</tbody></table>
<p class="note">{{.Pack.Decisions.Note}} The machine-readable pack carries {{len .Pack.Decisions.Items}} decisions with their full canonical inputs; they are not printed here because they are not legible.
{{if .Pack.Decisions.Truncated}}<strong>{{.Pack.Decisions.Message}}.</strong>{{end}}</p>

<h2>7 · Evidence attached to each artifact</h2>
<p class="note">An index, not a copy: the attestations are in the registry against the artifact's digest and are read
with anything that speaks OCI referrers. The last column is what the platform's own policy last concluded, which is
the newest scan and not a list.</p>
<table><thead><tr><th>Release</th><th>Digest</th><th>Attached</th><th>Gates</th><th>Newest scan</th></tr></thead><tbody>
{{range .Pack.Attestations}}<tr>
  <td>{{.Release}}{{if .Environments}}<br><span class="empty">running on {{join .Environments}}</span>{{end}}</td>
  <td class="mono">{{short (orDash .Digest)}}</td>
  <td>{{if .Evidence}}{{range .Evidence}}{{.PredicateType}} <span class="empty">({{orDash .Source}})</span><br>{{end}}{{else}}<span class="empty">nothing — {{orDash .Message}}</span>{{end}}</td>
  <td>{{if .Gates}}{{range .Gates}}{{.Name}}: {{orDash .Phase}}<br>{{end}}{{else}}<span class="empty">none ran</span>{{end}}</td>
  <td>{{with .NewestScan}}{{stamp .ScannedAt}}<br>{{orDash .Verdict}}<br><span class="empty">{{orDash .DataSnapshot}}</span>{{else}}<span class="empty">never re-evaluated</span>{{end}}</td>
</tr>{{else}}<tr><td colspan="5" class="empty">no artifacts in this window</td></tr>{{end}}
</tbody></table>

<h2>8 · Break-glass exceptions</h2>
<table><thead><tr><th>Name</th><th>Environment</th><th>Rules waived</th><th>Asked by</th><th>Approved by</th><th>Expires</th><th>At range end</th></tr></thead><tbody>
{{range .Pack.Exceptions}}<tr>
  <td>{{.Name}}{{if .IncidentRef}}<br><span class="empty">{{.IncidentRef}}</span>{{end}}</td>
  <td>{{.Environment}}</td><td class="mono">{{join .RuleIDs}}</td>
  <td>{{orDash .RequestedBy}}</td><td>{{orDash .ApprovedBy}}</td>
  <td>{{stamp .ExpiresAt}}</td>
  <td>{{.Phase}}{{if .UsedBy}}<br><span class="empty">relied on by {{join .UsedBy}}</span>{{end}}</td>
</tr>{{else}}<tr><td colspan="7" class="empty">no exception touched this window</td></tr>{{end}}
</tbody></table>
<p class="note">Every grant here is a rule somebody chose to waive, with a reason, an approver and an expiry.
Reasons, in order: {{range .Pack.Exceptions}}<em>{{.Name}}</em> — {{.Reason}}. {{end}}</p>

<h2>9 · What is running that no longer meets its bar</h2>
<table><thead><tr><th>Environment</th><th>Release</th><th>Status</th><th>Last re-evaluated</th><th>Against</th><th>Rules</th></tr></thead><tbody>
{{range .Pack.Drift.Current}}<tr>
  <td>{{.Environment}}</td><td>{{.Release}}</td>
  <td>{{.Status}}<br><span class="empty">{{.Message}}</span></td>
  <td>{{maybe .ScannedAt}}</td><td class="mono">{{orDash .DataSnapshot}}</td>
  <td>{{range .Rules}}{{.Rule}} <span class="empty">({{.Since}}{{if .WaivedAtPromotion}}, was waived by {{.WaivedAtPromotion}}{{end}})</span><br>{{else}}<span class="empty">none</span>{{end}}</td>
</tr>{{else}}<tr><td colspan="6" class="empty">nothing is deployed</td></tr>{{end}}
</tbody></table>
<p class="note">{{.Pack.Drift.Note}} The window holds {{len .Pack.Drift.History}} stored re-evaluations.</p>

<h2>10 · The audit log for this project</h2>
<p class="note">{{.Pack.AuditLog.Note}} {{.Pack.AuditLog.Privileged}} of the {{len .Pack.AuditLog.Items}} records below moved a control rather than a workload.
{{if .Pack.AuditLog.Truncated}}<strong>{{.Pack.AuditLog.Message}}.</strong>{{end}}</p>
<table><thead><tr><th>#</th><th>When</th><th>Who</th><th>What</th><th>Hash</th></tr></thead><tbody>
{{range .Pack.AuditLog.Items}}<tr>
  <td class="mono">{{.Sequence}}</td><td>{{stamp .Timestamp}}</td>
  <td>{{.Actor}} <span class="empty">({{.ActorKind}})</span></td>
  <td>{{.Operation}} {{.Kind}} {{.Name}}{{if .Privileged}} <strong>[{{orDash .PrivilegeClass}}]</strong>{{end}}<br><span class="empty">{{.Reason}}</span></td>
  <td class="mono">{{short .Hash}}<br><span class="empty">← {{short .PrevHash}}</span></td>
</tr>{{else}}<tr><td colspan="5" class="empty">no records for this project in this window</td></tr>{{end}}
</tbody></table>
<p class="note">The chain ends at sequence {{.Pack.AuditLog.Anchor}} according to an object outside the table.</p>

<h2>11 · Signed statements carried whole</h2>
<p class="note">{{.Pack.SignedRecords.Note}}</p>
<table><thead><tr><th>When</th><th>What it asserts</th><th>About</th><th>Record</th></tr></thead><tbody>
{{range .Pack.SignedRecords.Items}}<tr>
  <td>{{stamp .Timestamp}}</td><td class="mono">{{.Type}}</td>
  <td class="mono">{{short .Subject}}</td><td class="mono">{{.ID}}</td>
</tr>{{else}}<tr><td colspan="4" class="empty">no signed statements — see the machine-readable pack's verification block for why that may be</td></tr>{{end}}
</tbody></table>
<p class="note">The envelopes themselves are in the machine-readable pack under <code>signedRecords[].envelope</code>.</p>

</main></body></html>
`))
