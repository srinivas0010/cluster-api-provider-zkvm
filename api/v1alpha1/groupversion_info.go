

// +groupName=infrastructure.cluster.x-k8s.io

// Package v1alpha1 contains the v1alpha1 API types for the zkvm infrastructure
// provider.
//
// API group: infrastructure.cluster.x-k8s.io
// Version:   v1alpha1
//
// Resources in this group:
//   ZKVMCluster           — represents the KVM host environment for a cluster
//   ZKVMMachine           — represents one KVM virtual machine (one cluster node)
//   ZKVMMachineTemplate   — reusable VM spec blueprint referenced by KubeadmControlPlane
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group + version used to register the zkvm API types.
	// The group follows the CAPI convention: infrastructure.cluster.x-k8s.io
	GroupVersion = schema.GroupVersion{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1alpha1",
	}

	// SchemeBuilder is used to register the zkvm types with the controller-runtime scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the zkvm types to the given scheme.
	// Called from main.go so the controller-manager knows about these types.
	AddToScheme = SchemeBuilder.AddToScheme
)
