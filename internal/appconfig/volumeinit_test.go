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

package appconfig

import (
	"strings"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

func TestVolumeInitTakesTheTwoStepsAndNothingElse(t *testing.T) {
	inits, err := VolumeInits([]VolumeInit{{
		Volume:      "config",
		Directories: []VolumeInitDirectory{{Path: "data"}, {Path: "custom/deep", Mode: "0750"}},
		Seed:        []VolumeInitSeed{{File: "configuration", Path: "configuration.yaml"}},
	}}, "the web process")
	if err != nil {
		t.Fatal(err)
	}
	if len(inits) != 1 || inits[0].Volume != "config" {
		t.Fatalf("the declaration did not survive: %+v", inits)
	}
	if len(inits[0].Directories) != 2 || inits[0].Directories[1].Mode != "0750" {
		t.Errorf("the directories did not survive: %+v", inits[0].Directories)
	}
	if len(inits[0].Seed) != 1 || inits[0].Seed[0].File != "configuration" {
		t.Errorf("the seed did not survive: %+v", inits[0].Seed)
	}
}

// A path that leaves the volume is not spellable, which is the property the
// pattern exists for: there is no check here anybody could forget to write.
func TestVolumeInitRefusesAPathThatLeavesTheVolume(t *testing.T) {
	for _, path := range []string{"/etc/passwd", "../../etc", "data/../../etc", ".", "data/", ""} {
		_, err := VolumeInits([]VolumeInit{{
			Volume:      "config",
			Directories: []VolumeInitDirectory{{Path: path}},
		}}, "the web process")
		if err == nil {
			t.Errorf("%q was accepted as a path inside a volume", path)
		}
	}
}

func TestVolumeInitRefusesTheThingsThatWouldReadBackAndDoNothing(t *testing.T) {
	cases := []struct {
		name    string
		request []VolumeInit
		says    string
	}{
		{
			name:    "a volume with no steps",
			request: []VolumeInit{{Volume: "config"}},
			says:    "says nothing to do",
		},
		{
			name:    "an entry naming no volume",
			request: []VolumeInit{{Volume: "", Directories: []VolumeInitDirectory{{Path: "data"}}}},
			says:    "without saying which",
		},
		{
			name: "one volume twice",
			request: []VolumeInit{
				{Volume: "config", Directories: []VolumeInitDirectory{{Path: "a"}}},
				{Volume: "config", Directories: []VolumeInitDirectory{{Path: "b"}}},
			},
			says: "twice",
		},
		{
			name: "one directory twice",
			request: []VolumeInit{{Volume: "config", Directories: []VolumeInitDirectory{
				{Path: "data"}, {Path: "data", Mode: "0700"},
			}}},
			says: "listed twice",
		},
		{
			name: "two files seeded to one path",
			request: []VolumeInit{{Volume: "config", Seed: []VolumeInitSeed{
				{File: "one", Path: "app.yaml"}, {File: "two", Path: "app.yaml"},
			}}},
			says: "one path is one file",
		},
		{
			name: "a decimal mode",
			request: []VolumeInit{{Volume: "config", Directories: []VolumeInitDirectory{
				{Path: "data", Mode: "750"},
			}}},
			says: "four octal digits",
		},
		{
			name: "a seed naming a path instead of a file",
			request: []VolumeInit{{Volume: "config", Seed: []VolumeInitSeed{
				{File: "/etc/app.yaml", Path: "app.yaml"},
			}}},
			says: "no usable file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := VolumeInits(tc.request, "the web process")
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal reads %q, and does not say %q", err, tc.says)
			}
		})
	}
}

// The refusals name the workload, because a unit has four of them and a
// sentence that did not say which would be useless.
func TestVolumeInitRefusalsNameTheWorkload(t *testing.T) {
	_, err := VolumeInits([]VolumeInit{{Volume: "config"}}, `process "worker"`)
	if err == nil || !strings.Contains(err.Error(), `process "worker"`) {
		t.Fatalf("the refusal does not name the workload: %v", err)
	}
}

func TestASeedNamesAFileTheProjectDeclares(t *testing.T) {
	inits, err := VolumeInits([]VolumeInit{{
		Volume: "config",
		Seed:   []VolumeInitSeed{{File: "configuration", Path: "configuration.yaml"}},
	}}, "the web process")
	if err != nil {
		t.Fatal(err)
	}

	err = ValidateSeededFiles(inits, nil, nil)
	if err == nil {
		t.Fatal("a seed from a file the project does not declare has to be refused at the write, " +
			"rather than surfacing later as an environment that will not deploy")
	}
	if !strings.Contains(err.Error(), "configuration") {
		t.Errorf("the refusal does not name the file: %v", err)
	}

	files := []kitchenv1alpha1.ConfigFile{{Name: "configuration", Content: "logger: info\n"}}
	if err := ValidateSeededFiles(inits, nil, files); err != nil {
		t.Fatalf("the declared file was not accepted: %v", err)
	}
	// And the same rule for a named workload's own declaration.
	processes := []kitchenv1alpha1.ProcessSpec{{Name: "worker", Init: inits}}
	if err := ValidateSeededFiles(nil, processes, nil); err == nil {
		t.Fatal("a worker's seed is checked against the same list the web process's is")
	}
}

// A file that exists only to be seeded has no path, because a mounted config
// file is read-only and would shadow the copy the application then owns.
func TestAFileMayHaveNoPathAndIsThenPlacedNowhere(t *testing.T) {
	files, err := Files([]File{{Name: "configuration", Content: ptr("logger: info\n")}}, nil, nil)
	if err != nil {
		t.Fatalf("a file with no path was refused: %v", err)
	}
	if len(files) != 1 || files[0].Path != "" {
		t.Fatalf("the file did not survive without a path: %+v", files)
	}
	// Two of them collide with nothing, since neither is mounted anywhere.
	_, err = Files([]File{
		{Name: "one", Content: ptr("a")},
		{Name: "two", Content: ptr("b")},
	}, nil, nil)
	if err != nil {
		t.Fatalf("two files placed in no container were treated as colliding: %v", err)
	}
	// A path that is set is still checked.
	if _, err := Files([]File{{Name: "one", Path: "relative.yaml", Content: ptr("a")}}, nil, nil); err == nil {
		t.Error("a path that is not absolute is still refused")
	}
}

func ptr(s string) *string { return &s }
