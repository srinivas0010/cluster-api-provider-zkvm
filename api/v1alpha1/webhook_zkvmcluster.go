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

// webhook_zkvmcluster.go — validation methods for ZKVMCluster.
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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ValidateCreate validates a new ZKVMCluster. Called by unit tests only.
func (r *ZKVMCluster) ValidateCreate() ([]string, error) {
	return r.validate(nil)
}

// ValidateUpdate validates changes to an existing ZKVMCluster. Called by unit tests only.
func (r *ZKVMCluster) ValidateUpdate(old runtime.Object) ([]string, error) {
	return r.validate(old.(*ZKVMCluster))
}

// ValidateDelete is a no-op. Called by unit tests only.
func (r *ZKVMCluster) ValidateDelete() ([]string, error) {
	return nil, nil
}

func (r *ZKVMCluster) validate(old *ZKVMCluster) ([]string, error) {
	var errs field.ErrorList

	if strings.TrimSpace(r.Spec.KVMHost) == "" {
		errs = append(errs, field.Required(
			field.NewPath("spec", "kvmHost"),
			"kvmHost must be a non-empty hostname or IP address",
		))
	}
	if strings.TrimSpace(r.Spec.StoragePool) == "" {
		errs = append(errs, field.Required(
			field.NewPath("spec", "storagePool"),
			"storagePool must be a non-empty libvirt pool name",
		))
	}
	if strings.TrimSpace(r.Spec.NetworkBridge) == "" {
		errs = append(errs, field.Required(
			field.NewPath("spec", "networkBridge"),
			"networkBridge must be a non-empty Linux bridge interface name",
		))
	}
	if !strings.HasPrefix(r.Spec.KernelPath, "/") {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "kernelPath"),
			r.Spec.KernelPath,
			"kernelPath must be an absolute path (start with /)",
		))
	}
	if !strings.HasPrefix(r.Spec.InitrdPath, "/") {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "initrdPath"),
			r.Spec.InitrdPath,
			"initrdPath must be an absolute path (start with /)",
		))
	}
	if strings.TrimSpace(r.Spec.IgnitionBaseURL) != "" {
		if !strings.HasPrefix(r.Spec.IgnitionBaseURL, "http://") && !strings.HasPrefix(r.Spec.IgnitionBaseURL, "https://") {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "ignitionBaseURL"),
				r.Spec.IgnitionBaseURL,
				"ignitionBaseURL must start with http:// or https://",
			))
		}
	}
	if strings.TrimSpace(r.Spec.ControlPlaneEndpoint.Host) == "" {
		errs = append(errs, field.Required(
			field.NewPath("spec", "controlPlaneEndpoint", "host"),
			"controlPlaneEndpoint.host must be set to the API server IP or hostname",
		))
	}
	if old != nil && old.Spec.KVMHost != "" && old.Spec.KVMHost != r.Spec.KVMHost {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec", "kvmHost"),
			fmt.Sprintf("kvmHost is immutable after creation (was %q, cannot change to %q)",
				old.Spec.KVMHost, r.Spec.KVMHost),
		))
	}

	if len(errs) > 0 {
		return nil, apierrors.NewInvalid(r.GroupVersionKind().GroupKind(), r.Name, errs)
	}
	return nil, nil
}
