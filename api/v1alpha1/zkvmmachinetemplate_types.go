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
)

// ZKVMMachineTemplateResource defines a single ZKVMMachineTemplate entry.
// It wraps the ZKVMMachineSpec so KubeadmControlPlane and MachineDeployment
// can reference a reusable VM blueprint.
type ZKVMMachineTemplateResource struct {
	// Spec is the ZKVMMachineSpec that all machines created from this template
	// will use. Fields here are copied directly into each ZKVMMachine created.
	Spec ZKVMMachineSpec `json:"spec"`
}

// ZKVMMachineTemplateSpec defines the desired state of ZKVMMachineTemplate.
type ZKVMMachineTemplateSpec struct {
	// Template is the ZKVMMachineSpec blueprint.
	// +kubebuilder:validation:Required
	Template ZKVMMachineTemplateResource `json:"template"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=zkvmmachinetemplates,scope=Namespaced,categories=cluster-api
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ZKVMMachineTemplate is the schema for the zkvmmachinetemplates API.
// It is a reusable blueprint that KubeadmControlPlane and MachineDeployment
// reference via machineTemplate.infrastructureRef.
// Defines the VM size (vcpus, memoryGB, diskGB) for a group of cluster nodes.
type ZKVMMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ZKVMMachineTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ZKVMMachineTemplateList contains a list of ZKVMMachineTemplate objects.
type ZKVMMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZKVMMachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZKVMMachineTemplate{}, &ZKVMMachineTemplateList{})
}
