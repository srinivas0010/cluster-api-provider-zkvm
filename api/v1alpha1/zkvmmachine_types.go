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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// +kubebuilder:object:generate=true
// ZKVMMachineSpec defines the desired state of ZKVMMachine.
// Each ZKVMMachine corresponds to exactly one KVM virtual machine on the KVM host.
// ZKVMMachine objects are created automatically by KubeadmControlPlane or
// MachineDeployment — users do not create them directly.
type ZKVMMachineSpec struct {
	// VCPUs is the number of virtual CPUs to assign to the VM.
	// Minimum 4 for control-plane nodes, 2 for workers.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Required
	VCPUs int `json:"vcpus"`

	// MemoryGB is the amount of RAM in gigabytes to assign to the VM.
	// Minimum 16 for control-plane nodes, 8 for workers.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Required
	MemoryGB int `json:"memoryGB"`

	// DiskGB is the size of the root disk image (qcow2) in gigabytes.
	// Minimum 120 for control-plane nodes, 50 for workers.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Required
	DiskGB int `json:"diskGB"`

	// IPAddress is the static IP to assign to the VM if static NAT IP allocation is used.
	// +optional
	IPAddress string `json:"ipAddress,omitempty"`

	// Gateway is the network gateway for static IP configuration.
	// +optional
	Gateway string `json:"gateway,omitempty"`

	// Netmask is the network mask for static IP configuration (e.g. "255.255.255.0").
	// +optional
	Netmask string `json:"netmask,omitempty"`

	// Nameserver is the DNS nameserver for static IP configuration.
	// +optional
	Nameserver string `json:"nameserver,omitempty"`

	// ProviderID is the unique identifier for this machine in the format
	// "zkvm://<kvmHost>/<domainName>". Set by the reconciler after the VM
	// is created. Used by the Node object to match back to this Machine.
	// +optional
	ProviderID *string `json:"providerID,omitempty"`
}

// +kubebuilder:object:generate=true
// ZKVMMachineStatus defines the observed state of ZKVMMachine.
type ZKVMMachineStatus struct {
	// Ready indicates whether the VM has been created and has joined the cluster.
	// Set to true by the reconciler after DomainCreate succeeds and the node
	// appears in the target cluster.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Addresses holds the IP addresses assigned to this VM.
	// Populated after the VM is created and its NIC is assigned.
	// +optional
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// FailureReason contains a short machine-readable string describing why
	// this machine failed (e.g. "VolumeCreateFailed", "DomainStartFailed").
	// +optional
	FailureReason *string `json:"failureReason,omitempty"`

	// FailureMessage contains a human-readable explanation of the failure.
	// +optional
	FailureMessage *string `json:"failureMessage,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=zkvmmachines,scope=Namespaced,categories=cluster-api
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels['cluster\\.x-k8s\\.io/cluster-name']"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="ProviderID",type="string",JSONPath=".spec.providerID"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ZKVMMachine is the schema for the zkvmmachines API.
// Each ZKVMMachine maps 1:1 to a libvirt KVM virtual machine on the KVM host.
// Created automatically by KubeadmControlPlane/MachineDeployment — not by users.
type ZKVMMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZKVMMachineSpec   `json:"spec,omitempty"`
	Status ZKVMMachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZKVMMachineList contains a list of ZKVMMachine objects.
type ZKVMMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZKVMMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZKVMMachine{}, &ZKVMMachineList{})
}
