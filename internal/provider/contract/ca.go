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

package contract

// Where a claim's certificate authority lands in the pods that read it
// (#456).
//
// A binding carries the certificate itself — #398 for a database, #433 for
// the object store — because an application pod cannot mount a Secret in the
// platform's own namespace and no image the platform did not build carries a
// root the platform generated. That made verification *possible*, and left
// every application to do it: both Postgres drivers read `sslrootcert` as a
// **file**, and `AWS_CA_BUNDLE` names a file too, so a certificate handed
// over as a variable is a certificate the application has to write to disk
// before anything can verify against it.
//
// So the platform writes it. The Environment reconciler mounts the CA key of
// every binding Secret its workloads read, at the path below, in every
// workload the claim's variables reach; the claim contract composes the
// binding to name that same path — `sslmode=verify-full&sslrootcert=…` in a
// database URL, `caCertFile` beside `caCert` for the object store. The two
// halves are one decision and they are spelled here once, because a URL
// naming a path that is not mounted is worse than one that names none.
//
// **The path is the platform's, not the provider's.** A provisioner says
// *what* the authority is; only the platform knows where it will land, and
// only it knows the claim's name — which is what keeps two claims' authorities
// out of each other's way and keeps a preview's own branch at the same path as
// production's, so nothing in the binding moves when an environment does.
const (
	// CAMountRoot is the directory the platform places claim certificates
	// under. `/var/run` because this is machine-placed, read-only and gone
	// with the pod; a subdirectory per claim because a workload may read
	// several claims and each brings its own authority.
	//
	// It is constant and not configurable on purpose: it is inside somebody
	// else's image, so the only safe answer is a path nothing else uses, and
	// a per-installation one would be a path no binding could name.
	CAMountRoot = "/var/run/kitchen/claims"

	// CAFileName is what the certificate is called inside a claim's
	// directory — the name CloudNativePG, cert-manager and Kubernetes all
	// give a CA bundle in a Secret.
	CAFileName = "ca.crt"
)

// The binding-Secret keys a certificate authority travels in, spelled once
// here because the two halves — the contract that writes them and the
// Environment reconciler that mounts them — must agree exactly.
const (
	// BindingKeyCA is a database binding's authority (#398).
	BindingKeyCA = "ca"
	// BindingKeyCACert is an object store binding's (#433).
	BindingKeyCACert = "caCert"
	// BindingKeyCACertFile is where an object store binding's authority is
	// mounted, as a path an S3 client is pointed at — `AWS_CA_BUNDLE` and
	// every client-side equivalent of it want a file. A database binding
	// needs no such key: its URL already names the file, which is what makes
	// `verify-full` cost the application nothing.
	BindingKeyCACertFile = "caCertFile"
)

// CADir is where one claim's certificate authority is mounted.
func CADir(claim string) string { return CAMountRoot + "/" + claim }

// CAFile is the certificate itself, as a binding names it and as a workload
// reading that claim finds it.
func CAFile(claim string) string { return CADir(claim) + "/" + CAFileName }

// BindingCAKey answers which key of a binding Secret holds a certificate
// authority, and "" when none does.
//
// Present-and-empty counts as none, which is the same reading every writer of
// these keys takes: an empty certificate is an authority that vouches for
// nothing, and the object store's address refresh writes the key empty
// precisely so that a store which *lost* its private certificate can say so.
func BindingCAKey(data map[string][]byte) string {
	for _, key := range []string{BindingKeyCA, BindingKeyCACert} {
		if len(data[key]) > 0 {
			return key
		}
	}
	return ""
}
