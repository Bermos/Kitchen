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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// A project's configuration files (#311): what software the platform did not
// build is configured by.
//
// Two properties are what these cases are for. The first is the ordinary one
// — a declaration goes in, reads back, and can be edited by a client that
// read it. The second is the credential rule the whole platform is built on,
// applied one noun along: a **secret** file's content goes in and never comes
// back out, by any route, in any response, or into the audit log.

const (
	filesProject = "shop"
	projectPath  = "/api/v1/projects/" + filesProject
	filesPath    = projectPath + "/files"
	// theContent is the credential every secret-file test writes. It is a
	// distinctive string, so that "does this response, or this record,
	// contain it" is a substring search over the whole thing rather than a
	// walk of fields.
	theContent = "[server]\nSECRET_KEY = hunter2-correct-horse-battery-staple\n"
	// theFile is the name most of these declare it under.
	theFile = "app-ini"
)

// storedFiles reads the Secret the API writes a project's secret file content
// into — the one the ProjectReconciler mirrors from.
func storedFiles(t *testing.T, h *harness) map[string][]byte {
	t.Helper()
	secret, err := getSecret(t, h, controller.ProjectFilesSourceName(filesProject))
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return secret.Data
}

// declaredFiles is what the project's settings answer about its files.
func declaredFiles(t *testing.T, h *harness) []configFileView {
	t.Helper()
	recorder := h.do(t, http.MethodGet, projectPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("reading the project: %d %s", recorder.Code, recorder.Body.String())
	}
	return decode[projectView](t, recorder).Files
}

func TestDeclaringAPlainFileStoresItAndReadsItBack(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	const content = "logger: info\n"
	recorder := h.do(t, http.MethodPatch, projectPath, `{"files":[
		{"name":"configuration","path":"/config/configuration.yaml","content":`+
		quote(content)+`,"workloads":["web"]}
	]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	files := declaredFiles(t, h)
	if len(files) != 1 {
		t.Fatalf("want one file, got %+v", files)
	}
	// A plain file's content is not a credential, and an editor that could
	// not read it back would be an editor that could only replace it.
	if files[0].Content == nil || *files[0].Content != content {
		t.Fatalf("the content does not read back: %+v", files[0])
	}
	if files[0].Path != "/config/configuration.yaml" || files[0].Workloads[0] != kitchenv1alpha1.WebProcessName {
		t.Fatalf("the declaration does not read back: %+v", files[0])
	}
	if data := storedFiles(t, h); data != nil {
		t.Fatalf("a plain file's content went into the credential store: %v", data)
	}
}

func TestASecretFilesContentIsNeverAnsweredBack(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPatch, projectPath, `{"files":[
		{"name":"`+theFile+`","path":"/data/conf/app.ini","secret":true}
	]}`); recorder.Code != http.StatusOK {
		t.Fatalf("declaring the file: %d %s", recorder.Code, recorder.Body.String())
	}

	// Declared and not yet written, which is a state a screen has to be able
	// to name: the workloads that mount it will not start until it is.
	if files := declaredFiles(t, h); len(files) != 1 || files[0].ContentHash != "" || files[0].Content != nil {
		t.Fatalf("a file with no content yet should say so: %+v", files)
	}

	recorder := h.do(t, http.MethodPut, filesPath+"/"+theFile, `{"content":`+quote(theContent)+`}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "SECRET_KEY") {
		t.Fatalf("the response carried the content back: %s", recorder.Body.String())
	}
	written := decode[configFileView](t, recorder)
	if written.ContentHash == "" || written.Size != len(theContent) {
		t.Fatalf("the answer says nothing about what was stored: %+v", written)
	}

	// It reached the Secret the ProjectReconciler mirrors from, which is what
	// makes it reach the application.
	if got := string(storedFiles(t, h)[theFile]); got != theContent {
		t.Fatalf("the platform namespace holds %q, want the content that was written", got)
	}

	reading := h.do(t, http.MethodGet, projectPath, "")
	if strings.Contains(reading.Body.String(), "SECRET_KEY") {
		t.Fatalf("reading the project answered the content: %s", reading.Body.String())
	}
	files := declaredFiles(t, h)
	if len(files) != 1 || files[0].Content != nil {
		t.Fatalf("a secret file must never carry content: %+v", files)
	}
	if files[0].ContentHash != written.ContentHash || files[0].Size != len(theContent) {
		t.Fatalf("the project does not report what the platform holds: %+v", files[0])
	}

	// Writing it again is the same request with a different history, and it
	// says which it was.
	again := h.do(t, http.MethodPut, filesPath+"/"+theFile, `{"content":"[server]\nSECRET_KEY = two\n"}`)
	if again.Code != http.StatusOK {
		t.Fatalf("want 200 on a replacement, got %d: %s", again.Code, again.Body.String())
	}
	if decode[configFileView](t, again).ContentHash == written.ContentHash {
		t.Fatalf("replacing the content left the digest where it was")
	}
}

// The read-modify-write an editor makes: read the list, change one entry,
// send the rest back. It has nothing to send for a secret file's content, so
// an absent `content` has to keep what is stored — otherwise the first save
// of any other setting empties the credential.
func TestSendingTheFileListBackKeepsWhatItCouldNotRead(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	const plain = "logger: info\n"
	if recorder := h.do(t, http.MethodPatch, projectPath, `{"files":[
		{"name":"configuration","path":"/config/configuration.yaml","content":`+quote(plain)+`},
		{"name":"`+theFile+`","path":"/data/conf/app.ini","secret":true}
	]}`); recorder.Code != http.StatusOK {
		t.Fatalf("declaring the files: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodPut, filesPath+"/"+theFile,
		`{"content":`+quote(theContent)+`}`); recorder.Code != http.StatusCreated {
		t.Fatalf("writing the content: %d %s", recorder.Code, recorder.Body.String())
	}

	// One file moves; nothing else carries content at all.
	if recorder := h.do(t, http.MethodPatch, projectPath, `{"files":[
		{"name":"configuration","path":"/etc/app/configuration.yaml"},
		{"name":"`+theFile+`","path":"/data/conf/app.ini","secret":true}
	]}`); recorder.Code != http.StatusOK {
		t.Fatalf("moving one file: %d %s", recorder.Code, recorder.Body.String())
	}

	files := declaredFiles(t, h)
	if len(files) != 2 {
		t.Fatalf("want both files, got %+v", files)
	}
	if files[0].Path != "/etc/app/configuration.yaml" {
		t.Fatalf("the file did not move: %+v", files[0])
	}
	if files[0].Content == nil || *files[0].Content != plain {
		t.Fatalf("a plain file whose content the request left out lost it: %+v", files[0])
	}
	if got := string(storedFiles(t, h)[theFile]); got != theContent {
		t.Fatalf("the secret file's content was lost by a write that never saw it, holds %q", got)
	}
}

// A credential outliving the declaration that named it is residue nobody
// asked for, and residue nobody can see.
func TestRemovingASecretFileTakesItsContentWithIt(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPatch, projectPath, `{"files":[
		{"name":"`+theFile+`","path":"/data/conf/app.ini","secret":true}
	]}`); recorder.Code != http.StatusOK {
		t.Fatalf("declaring the file: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodPut, filesPath+"/"+theFile,
		`{"content":`+quote(theContent)+`}`); recorder.Code != http.StatusCreated {
		t.Fatalf("writing the content: %d %s", recorder.Code, recorder.Body.String())
	}

	if recorder := h.do(t, http.MethodPatch, projectPath, `{"files":[]}`); recorder.Code != http.StatusOK {
		t.Fatalf("removing the file: %d %s", recorder.Code, recorder.Body.String())
	}
	if data := storedFiles(t, h); len(data) != 0 {
		t.Fatalf("the content outlived the declaration: %v", data)
	}
	if files := declaredFiles(t, h); len(files) != 0 {
		t.Fatalf("the file is still declared: %+v", files)
	}
}

func TestASecretFilesContentRefusesWhatItCannotStore(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPatch, projectPath, `{"files":[
		{"name":"`+theFile+`","path":"/data/conf/app.ini","secret":true},
		{"name":"configuration","path":"/config/configuration.yaml","content":"logger: info\n"}
	]}`); recorder.Code != http.StatusOK {
		t.Fatalf("declaring the files: %d %s", recorder.Code, recorder.Body.String())
	}

	for name, spec := range map[string]struct {
		file, body string
		want       int
	}{
		"a file the project does not declare": {"nowhere", `{"content":"x"}`, http.StatusNotFound},
		"a file that is not secret":           {"configuration", `{"content":"x"}`, http.StatusBadRequest},
		"no content at all":                   {theFile, `{}`, http.StatusBadRequest},
		"empty content":                       {theFile, `{"content":""}`, http.StatusBadRequest},
		"more than the platform stores": {
			theFile,
			`{"content":"` + strings.Repeat("x", projectFileContentLimit+1) + `"}`,
			http.StatusBadRequest,
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPut, filesPath+"/"+spec.file, spec.body)
			if recorder.Code != spec.want {
				t.Fatalf("want %d, got %d: %s", spec.want, recorder.Code, recorder.Body.String())
			}
		})
	}
	if data := storedFiles(t, h); data != nil {
		t.Fatalf("a refused write stored something: %v", data)
	}
}

// Every refusal names the file it is about. A settings PATCH replaces a whole
// list, so "one of these is wrong" is not an answer anybody can act on.
func TestARefusedFileNamesItself(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, body := range map[string]string{
		"a path that is not absolute": `{"files":[
			{"name":"conf","path":"config/app.yaml","content":"a"}]}`,
		"a path naming a directory": `{"files":[
			{"name":"conf","path":"/config/","content":"a"}]}`,
		"a path reaching upwards": `{"files":[
			{"name":"conf","path":"/config/../../etc/passwd","content":"a"}]}`,
		"a name that is not a key": `{"files":[
			{"name":"not a key","path":"/config/app.yaml","content":"a"}]}`,
		"a workload the project does not declare": `{"files":[
			{"name":"conf","path":"/config/app.yaml","content":"a","workloads":["ghost"]}]}`,
		"two files at one path": `{"files":[
			{"name":"conf","path":"/config/app.yaml","content":"a"},
			{"name":"other","path":"/config/app.yaml","content":"b"}]}`,
		"one file twice": `{"files":[
			{"name":"conf","path":"/config/app.yaml","content":"a"},
			{"name":"conf","path":"/config/other.yaml","content":"b"}]}`,
		"a secret file carrying its content": `{"files":[
			{"name":"conf","path":"/config/app.yaml","secret":true,"content":"a"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, projectPath, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "conf") &&
				!strings.Contains(recorder.Body.String(), "not a key") {
				t.Fatalf("the refusal does not name the file: %s", recorder.Body.String())
			}
		})
	}
	if files := declaredFiles(t, h); len(files) != 0 {
		t.Fatalf("a refused settings write stored something: %+v", files)
	}
}

// A file may name a workload the same request declares: the names it may
// mention are the ones the project will have when the write lands.
func TestAFileMayNameAWorkloadTheSameRequestAdds(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, projectPath, `{
		"processes":[{"name":"worker","type":"worker","command":["./worker"]}],
		"files":[{"name":"conf","path":"/etc/worker.toml","content":"a","workloads":["worker"]}]
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// The property the whole feature rests on: the log records *that* content
// changed, and never the content. It is checked over the marshalled
// transition rather than field by field, because a field added later would
// otherwise be a leak nobody notices.
func TestTheAuditRecordOfASecretFileCarriesNoContent(t *testing.T) {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: filesProject, Namespace: testNamespace},
	}
	for _, operation := range []string{clickhouse.AuditCreate, clickhouse.AuditUpdate} {
		t.Run(operation, func(t *testing.T) {
			transition := projectFileTransition(project, theFile, operation)
			if transition.Kind != "ProjectFile" || transition.Project != filesProject {
				t.Fatalf("the record is not about the project's files: %+v", transition)
			}
			// A credential write, so it is in the same `?privileged=true`
			// view as a connection's rotation and a project secret's.
			if transition.Privileged != "credential" {
				t.Fatalf("the record is not classified as a credential write: %q", transition.Privileged)
			}
			encoded, err := json.Marshal(map[string]any{
				"reason":  transition.Reason,
				"details": transition.Details,
				"from":    transition.From,
				"to":      transition.To,
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "SECRET_KEY") {
				t.Fatalf("the record carries the content: %s", encoded)
			}
			if !strings.Contains(string(encoded), theFile) {
				t.Fatalf("the record does not say which file changed: %s", encoded)
			}
		})
	}
}

// quote renders a Go string as a JSON string, so a test body can carry a file
// with newlines in it without hand-escaping.
func quote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
