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

// webhook_zkvmmachine.go — validation methods for ZKVMMachine.
//
// These are plain methods, NOT registered as admission webhooks.
// SetupWebhookWithManager is intentionally absent — registering a webhook
// requires TLS certs at /tmp/k8s-webhook-server/serving-certs/tls.crt which
// are not provisioned in this deployment. Field validation is handled by
// CRD OpenAPI schema (kubebuilder markers) enforced by the API server.
//
// The Validate* methods are kept for unit testing only.

import (
	"fmt"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ValidateCreate validates a new ZKVMMachine. Called by unit tests only.
func (r *ZKVMMachine) ValidateCreate() ([]string, error) {
	return r.validate(nil)
}

// ValidateUpdate validates changes to an existing ZKVMMachine. Called by unit tests only.
func (r *ZKVMMachine) ValidateUpdate(old runtime.Object) ([]string, error) {
	return r.validate(old.(*ZKVMMachine))
}

// ValidateDelete is a no-op. Called by unit tests only.
func (r *ZKVMMachine) ValidateDelete() ([]string, error) {
	return nil, nil
}

func (r *ZKVMMachine) validate(old *ZKVMMachine) ([]string, error) {
	var errs field.ErrorList

	if r.Spec.VCPUs < 2 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "vcpus"), r.Spec.VCPUs,
			"vcpus must be >= 2 (minimum 4 recommended for control-plane nodes)"))
	}
	if r.Spec.MemoryGB < 8 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "memoryGB"), r.Spec.MemoryGB,
			"memoryGB must be >= 8 (minimum 16 recommended for control-plane nodes)"))
	}
	if r.Spec.DiskGB < 50 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "diskGB"), r.Spec.DiskGB,
			"diskGB must be >= 50 (minimum 120 recommended for control-plane nodes)"))
	}
	if r.Spec.IPAddress != "" {
		if ip := net.ParseIP(r.Spec.IPAddress); ip == nil || ip.To4() == nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "ipAddress"), r.Spec.IPAddress,
				"ipAddress must be a valid IPv4 address"))
		}
	}
	if r.Spec.Gateway != "" {
		if ip := net.ParseIP(r.Spec.Gateway); ip == nil || ip.To4() == nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "gateway"), r.Spec.Gateway,
				"gateway must be a valid IPv4 address"))
		}
	}
	if old != nil {
		if r.Spec.VCPUs < old.Spec.VCPUs {
			errs = append(errs, field.Forbidden(field.NewPath("spec", "vcpus"),
				fmt.Sprintf("vcpus cannot decrease (was %d, cannot change to %d)", old.Spec.VCPUs, r.Spec.VCPUs)))
		}
		if r.Spec.MemoryGB < old.Spec.MemoryGB {
			errs = append(errs, field.Forbidden(field.NewPath("spec", "memoryGB"),
				fmt.Sprintf("memoryGB cannot decrease (was %d, cannot change to %d)", old.Spec.MemoryGB, r.Spec.MemoryGB)))
		}
		if r.Spec.DiskGB < old.Spec.DiskGB {
			errs = append(errs, field.Forbidden(field.NewPath("spec", "diskGB"),
				fmt.Sprintf("diskGB cannot decrease (was %d, cannot change to %d)", old.Spec.DiskGB, r.Spec.DiskGB)))
		}
	}

	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(r.GroupVersionKind().GroupKind(), r.Name, errs)
	}
	return nil, nil
}
