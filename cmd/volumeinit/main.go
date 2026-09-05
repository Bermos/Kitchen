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

// Command volume-init prepares a workload's volumes before the workload
// starts (#348).
//
// It runs as an init container in the application's own pod, with that pod's
// volumes mounted and that project's security posture on it, and it does
// exactly what the plan in its environment says: create directories that are
// absent, copy in configuration files whose destination is absent. There is
// no argv from any request, no shell, and nothing it can be asked to run —
// the whole vocabulary is the two typed steps in internal/volumeinit.
//
// A failure is written to the pod's termination log naming the step and the
// reason, which is what the environment reports rather than leaving a
// workload that never becomes ready.
//
// It ships in the operator's image and is never run by a person.
package main

import (
	"fmt"
	"os"

	"github.com/Bermos/Kitchen/internal/volumeinit"
)

func main() {
	if err := run(); err != nil {
		fail(err)
	}
}

func run() error {
	plan, err := volumeinit.Parse(os.Getenv(volumeinit.PlanVariable))
	if err != nil {
		return err
	}
	step, err := volumeinit.Run(plan, volumeinit.SeedDir)
	if err != nil {
		return fmt.Errorf("%s: %w", step, err)
	}
	for _, volume := range plan.Volumes {
		fmt.Printf("prepared volume %s at %s: %d directories, %d seeded files\n",
			volume.Claim, volume.MountPath, len(volume.Directories), len(volume.Seeds))
	}
	return nil
}

// fail records why and stops the pod. The message goes to the termination log
// because that is the one channel whose content reaches the operator without
// anybody reading a log: the kubelet copies it into the container status, and
// the environment reconciler turns it into a condition naming the step.
//
// It goes to stderr as well, so that a pod being looked at directly says the
// same thing as the Environment does.
func fail(err error) {
	fmt.Fprintln(os.Stderr, err.Error())
	if writeErr := os.WriteFile(volumeinit.TerminationLog, []byte(err.Error()), 0o644); writeErr != nil {
		fmt.Fprintf(os.Stderr, "could not write the termination log: %v\n", writeErr)
	}
	os.Exit(1)
}
