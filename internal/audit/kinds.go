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

package audit

// The kinds transitions are recorded under.
//
// They are constants, and shared between the reconcilers and the REST API,
// for one reason that matters more than tidiness: the log is queried by kind,
// and a record written as "build" where every other says "Build" is a record
// the query for that object will never find. A typo in a log line is
// cosmetic; a typo here loses evidence.
//
// They are the Kubernetes kinds rather than a vocabulary of Kitchen's own, so
// that a record can be taken back to the object it is about without a
// translation table.
const (
	KindBuild          = "Build"
	KindConnection     = "Connection"
	KindDomain         = "Domain"
	KindEnvironment    = "Environment"
	KindKitchen        = "Kitchen"
	KindPlatformUpdate = "PlatformUpdate"
	KindProject        = "Project"
	KindRelease        = "Release"
	KindResourceClaim  = "ResourceClaim"
)
