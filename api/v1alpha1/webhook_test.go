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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// validCluster returns a ZKVMCluster with all required fields set.
func validCluster() *ZKVMCluster {
	return &ZKVMCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
		Spec: ZKVMClusterSpec{
			KVMHost:       "kvm-host.example.com",
			StoragePool:   "default",
			NetworkBridge: "br0",
			KernelPath:    "/var/www/html/agent.s390x-vmlinuz",
			InitrdPath:    "/var/www/html/agent.s390x-initrd.img",
			ControlPlaneEndpoint: clusterv1.APIEndpoint{
				Host: "10.0.0.10",
				Port: 6443,
			},
		},
	}
}

// TestZKVMClusterWebhook_ValidCreate verifies that a fully-specified cluster
// passes admission without errors.
func TestZKVMClusterWebhook_ValidCreate(t *testing.T) {
	c := validCluster()
	_, err := c.ValidateCreate()
	if err != nil {
		t.Errorf("expected no error for valid cluster, got: %v", err)
	}
}

// TestZKVMClusterWebhook_MissingFields verifies that missing required fields
// are caught at admission time.
func TestZKVMClusterWebhook_MissingFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ZKVMCluster)
		wantErr string
	}{
		{
			name:    "empty kvmHost",
			mutate:  func(c *ZKVMCluster) { c.Spec.KVMHost = "" },
			wantErr: "kvmHost",
		},
		{
			name:    "empty storagePool",
			mutate:  func(c *ZKVMCluster) { c.Spec.StoragePool = "" },
			wantErr: "storagePool",
		},
		{
			name:    "empty networkBridge",
			mutate:  func(c *ZKVMCluster) { c.Spec.NetworkBridge = "" },
			wantErr: "networkBridge",
		},
		{
			name:    "relative kernelPath",
			mutate:  func(c *ZKVMCluster) { c.Spec.KernelPath = "var/www/html/vmlinuz" },
			wantErr: "kernelPath",
		},
		{
			name:    "relative initrdPath",
			mutate:  func(c *ZKVMCluster) { c.Spec.InitrdPath = "var/www/html/initrd.img" },
			wantErr: "initrdPath",
		},
		{
			name:    "empty controlPlaneEndpoint host",
			mutate:  func(c *ZKVMCluster) { c.Spec.ControlPlaneEndpoint.Host = "" },
			wantErr: "controlPlaneEndpoint",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validCluster()
			tc.mutate(c)
			_, err := c.ValidateCreate()
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tc.wantErr)
				return
			}
			if msg := err.Error(); len(msg) == 0 {
				t.Errorf("expected non-empty error message")
			}
		})
	}
}

// TestZKVMClusterWebhook_KVMHostImmutable verifies that kvmHost cannot be
// changed on an existing cluster (would orphan all running VMs).
func TestZKVMClusterWebhook_KVMHostImmutable(t *testing.T) {
	old := validCluster()
	updated := validCluster()
	updated.Spec.KVMHost = "different-host.example.com"

	_, err := updated.ValidateUpdate(old)
	if err == nil {
		t.Error("expected error when changing kvmHost, got nil")
	}
}

// TestZKVMClusterWebhook_ValidUpdate verifies that non-immutable fields
// can be changed on update.
func TestZKVMClusterWebhook_ValidUpdate(t *testing.T) {
	old := validCluster()
	updated := validCluster()
	// Changing the kernel path on update is allowed (e.g. upgrading OCP version).
	updated.Spec.KernelPath = "/var/www/html/agent-4.17.s390x-vmlinuz"

	_, err := updated.ValidateUpdate(old)
	if err != nil {
		t.Errorf("expected no error for valid update, got: %v", err)
	}
}

// ── ZKVMMachine webhook tests ─────────────────────────────────────────────────

// validMachine returns a ZKVMMachine with all fields meeting the minimums.
func validMachine() *ZKVMMachine {
	return &ZKVMMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Namespace: "default"},
		Spec: ZKVMMachineSpec{
			VCPUs:    4,
			MemoryGB: 16,
			DiskGB:   120,
		},
	}
}

func TestZKVMMachineWebhook_ValidCreate(t *testing.T) {
	m := validMachine()
	_, err := m.ValidateCreate()
	if err != nil {
		t.Errorf("expected no error for valid machine, got: %v", err)
	}
}

func TestZKVMMachineWebhook_BelowMinimums(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ZKVMMachine)
		wantErr string
	}{
		{
			name:    "vcpus below 2",
			mutate:  func(m *ZKVMMachine) { m.Spec.VCPUs = 1 },
			wantErr: "vcpus",
		},
		{
			name:    "memoryGB below 8",
			mutate:  func(m *ZKVMMachine) { m.Spec.MemoryGB = 4 },
			wantErr: "memoryGB",
		},
		{
			name:    "diskGB below 50",
			mutate:  func(m *ZKVMMachine) { m.Spec.DiskGB = 20 },
			wantErr: "diskGB",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := validMachine()
			tc.mutate(m)
			_, err := m.ValidateCreate()
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tc.wantErr)
			}
		})
	}
}

func TestZKVMMachineWebhook_NoShrink(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ZKVMMachine)
	}{
		{
			name:   "shrink vcpus",
			mutate: func(m *ZKVMMachine) { m.Spec.VCPUs = 2 },
		},
		{
			name:   "shrink memoryGB",
			mutate: func(m *ZKVMMachine) { m.Spec.MemoryGB = 8 },
		},
		{
			name:   "shrink diskGB",
			mutate: func(m *ZKVMMachine) { m.Spec.DiskGB = 60 },
		},
	}

	old := validMachine() // VCPUs=4, MemoryGB=16, DiskGB=120

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			updated := validMachine()
			tc.mutate(updated)
			_, err := updated.ValidateUpdate(old)
			if err == nil {
				t.Errorf("expected error when shrinking %s, got nil", tc.name)
			}
		})
	}
}

func TestZKVMMachineWebhook_ValidUpdate_Grow(t *testing.T) {
	old := validMachine()
	updated := validMachine()
	updated.Spec.VCPUs = 8
	updated.Spec.MemoryGB = 32

	_, err := updated.ValidateUpdate(old)
	if err != nil {
		t.Errorf("expected no error when growing machine, got: %v", err)
	}
}
