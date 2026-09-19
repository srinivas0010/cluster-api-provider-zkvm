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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// SetupWebhookWithManager registers the ZKVMCluster webhook with the manager.
func (r *ZKVMCluster) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

// +kubebuilder:webhook:path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-zkvmcluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=infrastructure.cluster.x-k8s.io,resources=zkvmclusters,verbs=create;update,versions=v1alpha1,name=vzkvmcluster.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &ZKVMCluster{}

// ValidateCreate validates a new ZKVMCluster on admission.
func (r *ZKVMCluster) ValidateCreate() (admission.Warnings, error) {
	return r.validate(nil)
}

// ValidateUpdate validates changes to an existing ZKVMCluster on admission.
func (r *ZKVMCluster) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
	return r.validate(old.(*ZKVMCluster))
}

// ValidateDelete is a no-op — deletion is always permitted.
func (r *ZKVMCluster) ValidateDelete() (admission.Warnings, error) {
	return nil, nil
}

// validate enforces field-level invariants for ZKVMCluster.
//
// Bug 9 fix: kubebuilder +kubebuilder:validation markers in the types file
// generate CRD validation schemas enforced by the API server, but those only
// fire when the CRD CEL/schema rules run — not when the controller processes
// the object. An admission webhook fires at API request time, before the
// object is persisted, giving a clear error to the user immediately.
func (r *ZKVMCluster) validate(old *ZKVMCluster) (admission.Warnings, error) {
	var errs field.ErrorList

	// KVMHost must be non-empty.
	if strings.TrimSpace(r.Spec.KVMHost) == "" {
		errs = append(errs, field.Required(
			field.NewPath("spec", "kvmHost"),
			"kvmHost must be a non-empty hostname or IP address",
		))
	}

	// StoragePool must be non-empty.
	if strings.TrimSpace(r.Spec.StoragePool) == "" {
		errs = append(errs, field.Required(
			field.NewPath("spec", "storagePool"),
			"storagePool must be a non-empty libvirt pool name",
		))
	}

	// NetworkBridge must be non-empty.
	if strings.TrimSpace(r.Spec.NetworkBridge) == "" {
		errs = append(errs, field.Required(
			field.NewPath("spec", "networkBridge"),
			"networkBridge must be a non-empty Linux bridge interface name",
		))
	}

	// KernelPath must be an absolute path.
	if !strings.HasPrefix(r.Spec.KernelPath, "/") {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "kernelPath"),
			r.Spec.KernelPath,
			"kernelPath must be an absolute path (start with /)",
		))
	}

	// InitrdPath must be an absolute path.
	if !strings.HasPrefix(r.Spec.InitrdPath, "/") {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "initrdPath"),
			r.Spec.InitrdPath,
			"initrdPath must be an absolute path (start with /)",
		))
	}

	// IgnitionBaseURL is optional (ABI ignition is embedded in the initrd).
	// If provided, validate it is a valid http/https URL.
	if strings.TrimSpace(r.Spec.IgnitionBaseURL) != "" {
		if !strings.HasPrefix(r.Spec.IgnitionBaseURL, "http://") && !strings.HasPrefix(r.Spec.IgnitionBaseURL, "https://") {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "ignitionBaseURL"),
				r.Spec.IgnitionBaseURL,
				"ignitionBaseURL must start with http:// or https://",
			))
		}
	}

	// ControlPlaneEndpoint host must be non-empty.
	if strings.TrimSpace(r.Spec.ControlPlaneEndpoint.Host) == "" {
		errs = append(errs, field.Required(
			field.NewPath("spec", "controlPlaneEndpoint", "host"),
			"controlPlaneEndpoint.host must be set to the API server IP or hostname",
		))
	}

	// Immutability guard: KVMHost must not change once set.
	// Changing the KVM host mid-flight would orphan all running VMs.
	if old != nil && old.Spec.KVMHost != "" && old.Spec.KVMHost != r.Spec.KVMHost {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec", "kvmHost"),
			fmt.Sprintf(
				"kvmHost is immutable after creation (was %q, cannot change to %q)",
				old.Spec.KVMHost, r.Spec.KVMHost,
			),
		))
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
