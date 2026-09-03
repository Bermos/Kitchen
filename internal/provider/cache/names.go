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

package cache

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// The name budget. A StatefulSet's pods are its name plus "-0", and its
// Service's name has to be a DNS label, so the instance's name is capped
// well short of 63; internal/provider/naming is what keeps a name inside it
// without ever mapping two names onto one.
const maxInstanceName = 48

// branchName is a preview's instance beside the one it branches from. The
// environment's name goes in the hash rather than the head, because two
// previews of one claim differ at the end of a long name far more often
// than at the start.
func branchName(instanceID, environment string) string {
	_, parent, err := splitID(instanceID)
	if err != nil {
		parent = instanceID
	}
	return naming.Truncate(parent+"-"+environment, maxInstanceName)
}

// splitID takes an instance ID apart into the namespace and name it is
// made of. An ID that is not one is an error rather than a guess: it came
// off a claim's status, and acting on half of it would address the wrong
// object.
func splitID(instanceID string) (namespace, name string, err error) {
	parts := strings.SplitN(instanceID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%q is not a namespace/name instance id", instanceID)
	}
	return parts[0], parts[1], nil
}

func intstrFromInt(port int) intstr.IntOrString {
	return intstr.FromInt32(int32(port))
}
