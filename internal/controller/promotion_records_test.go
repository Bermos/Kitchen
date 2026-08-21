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

package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
)

// createAutomaticPromotion retries — a finished build's requeue, an applied
// stage's — and a retry that finds its own work done must add nothing to the
// audit trail. These tests probe that with a recorder that FAILS every
// record (no Kitchen singleton behind it): a record that should not happen
// becomes an error the test can see.

func TestAnAutomaticPromotionRetryRecordsNothingWhenItsWorkIsDone(t *testing.T) {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: PlatformNamespace},
	}
	existing := &kitchenv1alpha1.Promotion{
		ObjectMeta: metav1.ObjectMeta{
			Name:      automaticPromotionName("shop", "shop-rel-1", "shop-production"),
			Namespace: PlatformNamespace,
		},
	}
	c := complianceClient(t, project, existing)
	// No Kitchen singleton: this recorder fails every Record, so a CREATE
	// record for a promotion that already exists would fail the call.
	failing := &audit.Recorder{Client: c, Namespace: PlatformNamespace, Singleton: KitchenSingletonName}

	if err := createAutomaticPromotion(context.Background(), c, failing, actorBuildController,
		"corr", PlatformNamespace, project, "shop-production", "shop-rel-1",
		audit.ControllerActor(actorBuildController), "retry of a finished build"); err != nil {
		t.Fatalf("a retry that finds its promotion must record nothing and succeed: %v", err)
	}
}

func TestAFreshAutomaticPromotionIsRecordedBeforeItIsCreated(t *testing.T) {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: PlatformNamespace},
	}
	c := complianceClient(t, project)
	failing := &audit.Recorder{Client: c, Namespace: PlatformNamespace, Singleton: KitchenSingletonName}

	err := createAutomaticPromotion(context.Background(), c, failing, actorBuildController,
		"corr", PlatformNamespace, project, "shop-production", "shop-rel-1",
		audit.ControllerActor(actorBuildController), "a finished build")
	if err == nil {
		t.Fatal("a fresh promotion's CREATE record gates its creation; a refused record must fail the call")
	}

	// And nothing was created past the refused record.
	name := automaticPromotionName("shop", "shop-rel-1", "shop-production")
	getErr := c.Get(context.Background(),
		types.NamespacedName{Namespace: PlatformNamespace, Name: name}, &kitchenv1alpha1.Promotion{})
	if !apierrors.IsNotFound(getErr) {
		t.Fatalf("no promotion may exist that no record precedes, got %v", getErr)
	}
}
