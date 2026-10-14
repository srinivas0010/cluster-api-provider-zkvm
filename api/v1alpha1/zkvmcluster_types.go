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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// ZKVMClusterSpec defines the desired state of ZKVMCluster.
// It describes the KVM host environment that will back a CAPI-managed cluster.
type ZKVMClusterSpec struct {
	// KVMHost is the hostname or IP address of the KVM host LPAR.
	// The controller connects here via: qemu+ssh://<user>@<KVMHost>/system
	// +kubebuilder:validation:Required
	KVMHost string `json:"kvmHost"`

	// BastionIP is the IP address of the dedicated bastion VM for this cluster.
	// The bastion runs HAProxy (ports 6443, 22623, 443, 80), named (DNS), and
	// HTTPD (port 8080 for rootfs serving). It is a KVM VM on the same network
	// bridge as the target cluster nodes and is provisioned exclusively for
	// this cluster — it is not shared with any other cluster.
	// This field is informational — the installer configures the bastion before
	// applying CAPI manifests. The controller does not connect to the bastion.
	// +optional
	BastionIP string `json:"bastionIP,omitempty"`

	// SSHUser is the username used to SSH into the KVM host (e.g. "root").
	// +optional
	SSHUser string `json:"sshUser,omitempty"`

	// SSHKeySecretRef is a reference to a Secret containing the SSH private key
	// used to authenticate to the KVM host. The secret must contain key "ssh-privatekey".
	// +optional
	SSHKeySecretRef *corev1.SecretReference `json:"sshKeySecretRef,omitempty"`

	// StoragePool is the name of the libvirt storage pool where VM disk images
	// (qcow2) will be created. Must already exist on the KVM host.
	// Verify with: virsh pool-list --all
	// +kubebuilder:validation:Required
	StoragePool string `json:"storagePool"`

	// NetworkBridge is the name of the Linux bridge interface on the KVM host
	// that VM virtual NICs will be attached to.
	// Must be a bridge connected to the physical network (not virbr0/NAT).
	// Verify with: ip link show <bridge>
	// +kubebuilder:validation:Required
	NetworkBridge string `json:"networkBridge"`

	// ControlPlaneEndpoint is the IP and port of the Kubernetes API server
	// for the target cluster. This is the IP of the first control-plane VM.
	// +kubebuilder:validation:Required
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint"`

	// KernelPath is the absolute path on the KVM host to the s390x kernel image
	// used for direct-kernel boot of all VMs in this cluster.
	// Populated by the installer's UploadArtifacts step, e.g. "/var/lib/libvirt/boot/agent.s390x-kernel.img".
	// All VMs in the cluster boot the same kernel image.
	// +kubebuilder:validation:Required
	KernelPath string `json:"kernelPath"`

	// InitrdPath is the absolute path on the KVM host to the initrd image
	// used for direct-kernel boot of all VMs in this cluster.
	// Populated by the installer's UploadArtifacts step, e.g. "/var/lib/libvirt/boot/agent.s390x-initrd.img".
	// All VMs in the cluster use the same initrd image.
	// +kubebuilder:validation:Required
	InitrdPath string `json:"initrdPath"`

	// RootFSURL is the HTTP URL from which nodes fetch the live rootfs image
	// (agent.s390x-rootfs.img) during direct-kernel PXE boot.
	// e.g. "http://192.168.152.2:8080/rootfs.img".
	// Appended to the kernel cmdline as: coreos.live.rootfs_url=<RootFSURL>
	// +optional
	RootFSURL string `json:"rootFSURL,omitempty"`

	// IgnitionBaseURL is the base URL from which nodes fetch their ignition
	// configuration during first boot. Optional — ABI ignition is embedded
	// in the initrd and does not require an external URL. This field is
	// preserved for future use cases that may serve per-node ignition files.
	// +optional
	IgnitionBaseURL string `json:"ignitionBaseURL,omitempty"`

	// Day2Boot holds configuration for day-2 worker node replacement via RHCOS
	// live PXE boot. When set, the controller uses this block instead of the
	// ABI kernel/initrd for Machines that carry the deployment-name label
	// (i.e. workers created by a MachineDeployment).
	// Populated by the installer after install-complete via a patch call.
	// +optional
	Day2Boot *Day2BootConfig `json:"day2Boot,omitempty"`

	// WorkerIPPool is an ordered list of IPv4 addresses available for dynamic
	// assignment to worker ZKVMMachines that have no pre-set ipAddress
	// (i.e. machines created from a ZKVMMachineTemplate via MachineDeployment).
	//
	// The reconciler scans all ZKVMMachines in the cluster namespace, collects
	// IPs already in use, and claims the first unclaimed IP from this list for
	// each new worker machine. Add extra IPs beyond the initial replica count
	// to allow scale-up without reinstalling.
	//
	// Example: ip_start=192.168.153.20, ip_count=6 → pool is
	//   ["192.168.153.20","192.168.153.21","192.168.153.22",
	//    "192.168.153.23","192.168.153.24","192.168.153.25"]
	//
	// Not used for control-plane nodes — they always have a fixed ipAddress.
	// +optional
	WorkerIPPool []string `json:"workerIPPool,omitempty"`
}

// Day2BootConfig holds the RHCOS live PXE settings used for day-2 worker
// replacement. All fields are absolute paths or HTTP URLs on the KVM host.
type Day2BootConfig struct {
	// KernelPath is the absolute path on the KVM host to the RHCOS live kernel.
	// Must be readable by the QEMU process (not under /root).
	// e.g. "/var/lib/libvirt/boot/rhcos-9.8.20260428-0-live-kernel.s390x"
	KernelPath string `json:"kernelPath"`

	// InitrdPath is the absolute path on the KVM host to the RHCOS live initramfs.
	// e.g. "/var/lib/libvirt/boot/rhcos-9.8.20260428-0-live-initramfs.s390x.img"
	InitrdPath string `json:"initrdPath"`

	// RootFSURL is the HTTP URL from which the VM fetches the live rootfs.
	// e.g. "http://192.168.153.1:9100/rhcos-9.8.20260428-0-live-rootfs.s390x.img"
	RootFSURL string `json:"rootFSURL"`

	// HTTPBaseURL is the base URL of the HTTP server on the KVM host bridge.
	// The controller derives the ignition URL as HTTPBaseURL + "/worker.ign".
	// e.g. "http://192.168.153.1:9100"
	HTTPBaseURL string `json:"httpBaseURL"`

	// WebRoot is the absolute directory path on the KVM host that the HTTP
	// service serves. The controller writes worker.ign here before each new VM.
	// e.g. "/var/lib/libvirt/day2"
	WebRoot string `json:"webRoot"`
}

// ZKVMClusterStatus defines the observed state of ZKVMCluster.
type ZKVMClusterStatus struct {
	// Ready indicates whether the KVM host is reachable and the storage pool
	// and network bridge have been validated. Set to true by the reconciler
	// once all checks pass. CAPI core waits for this before creating machines.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// FailureReason contains a short machine-readable string explaining why
	// the cluster failed to reach the Ready state.
	// +optional
	FailureReason *string `json:"failureReason,omitempty"`

	// FailureMessage contains a human-readable description of the failure.
	// +optional
	FailureMessage *string `json:"failureMessage,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=zkvmclusters,scope=Namespaced,categories=cluster-api
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels['cluster\\.x-k8s\\.io/cluster-name']"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="KVMHost",type="string",JSONPath=".spec.kvmHost"
// +kubebuilder:printcolumn:name="BastionIP",type="string",JSONPath=".spec.bastionIP"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ZKVMCluster is the schema for the zkvmclusters API.
// It represents the KVM host environment (LPAR + libvirt) that backs a cluster.
// One ZKVMCluster exists per target cluster.
type ZKVMCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZKVMClusterSpec   `json:"spec,omitempty"`
	Status ZKVMClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZKVMClusterList contains a list of ZKVMCluster objects.
type ZKVMClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZKVMCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ZKVMCluster{}, &ZKVMClusterList{})
}
