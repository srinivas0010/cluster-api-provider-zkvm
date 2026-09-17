/*
Copyright 2024 IBM Corporation.

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

package v1alpha1

import (
	"fmt"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// SetupWebhookWithManager registers the ZKVMMachine webhook with the manager.
func (r *ZKVMMachine) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

// +kubebuilder:webhook:path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-zkvmmachine,mutating=false,failurePolicy=fail,sideEffects=None,groups=infrastructure.cluster.x-k8s.io,resources=zkvmmachines,verbs=create;update,versions=v1alpha1,name=vzkvmmachine.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &ZKVMMachine{}

// ValidateCreate validates a new ZKVMMachine on admission.
func (r *ZKVMMachine) ValidateCreate() (admission.Warnings, error) {
	return r.validate(nil)
}

// ValidateUpdate validates changes to an existing ZKVMMachine on admission.
func (r *ZKVMMachine) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
	return r.validate(old.(*ZKVMMachine))
}

// ValidateDelete is a no-op — deletion is always permitted.
func (r *ZKVMMachine) ValidateDelete() (admission.Warnings, error) {
	return nil, nil
}

// validate enforces numeric minimums declared in the type's kubebuilder markers.
//
// Bug 9 fix: +kubebuilder:validation:Minimum markers generate OpenAPI schema
// constraints, but an admission webhook provides richer error messages and
// enforces immutability rules that CEL cannot express as concisely.
func (r *ZKVMMachine) validate(old *ZKVMMachine) (admission.Warnings, error) {
	var errs field.ErrorList

	// VCPUs: minimum 1 (marker), but we also enforce a practical floor of 2.
	if r.Spec.VCPUs < 2 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "vcpus"),
			r.Spec.VCPUs,
			"vcpus must be >= 2 (minimum 4 recommended for control-plane nodes)",
		))
	}

	// MemoryGB: minimum 1 (marker), but we enforce >= 8 GB.
	if r.Spec.MemoryGB < 8 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "memoryGB"),
			r.Spec.MemoryGB,
			"memoryGB must be >= 8 (minimum 16 recommended for control-plane nodes)",
		))
	}

	// DiskGB: minimum 1 (marker), but we enforce >= 50 GB.
	if r.Spec.DiskGB < 50 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "diskGB"),
			r.Spec.DiskGB,
			"diskGB must be >= 50 (minimum 120 recommended for control-plane nodes)",
		))
	}

	// Validate IPAddress if provided
	if r.Spec.IPAddress != "" {
		if ip := net.ParseIP(r.Spec.IPAddress); ip == nil || ip.To4() == nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "ipAddress"),
				r.Spec.IPAddress,
				"ipAddress must be a valid IPv4 address",
			))
		}
	}

	// Validate Gateway if provided
	if r.Spec.Gateway != "" {
		if ip := net.ParseIP(r.Spec.Gateway); ip == nil || ip.To4() == nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "gateway"),
				r.Spec.Gateway,
				"gateway must be a valid IPv4 address",
			))
		}
	}

	// Immutability guard: VCPUs, MemoryGB, DiskGB must not shrink after creation.
	// Growing VMs is a future feature; shrinking risks data loss / OOM.
	if old != nil {
		if r.Spec.VCPUs < old.Spec.VCPUs {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec", "vcpus"),
				fmt.Sprintf("vcpus cannot decrease (was %d, cannot change to %d)", old.Spec.VCPUs, r.Spec.VCPUs),
			))
		}
		if r.Spec.MemoryGB < old.Spec.MemoryGB {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec", "memoryGB"),
				fmt.Sprintf("memoryGB cannot decrease (was %d, cannot change to %d)", old.Spec.MemoryGB, r.Spec.MemoryGB),
			))
		}
		if r.Spec.DiskGB < old.Spec.DiskGB {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec", "diskGB"),
				fmt.Sprintf("diskGB cannot decrease (was %d, cannot change to %d)", old.Spec.DiskGB, r.Spec.DiskGB),
			))
		}
	}

	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(
			r.GroupVersionKind().GroupKind(),
			r.Name,
			errs,
		)
	}

	return nil, nil
}
