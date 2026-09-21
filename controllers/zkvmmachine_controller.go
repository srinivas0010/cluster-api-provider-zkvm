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

package controllers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/openshift-smart-installer/cluster-api-provider-zkvm/api/v1alpha1"
	zkvmlibvirt "github.com/openshift-smart-installer/cluster-api-provider-zkvm/internal/libvirt"
)

const zkvmMachineFinalizer = "zkvmmachine.infrastructure.cluster.x-k8s.io"

// ZKVMMachineReconciler reconciles ZKVMMachine objects.
//
// Responsibilities:
//   - On create: look up the owning ZKVMCluster for KVM host details,
//     create a qcow2 disk volume, define the libvirt domain, start the VM.
//     Set ZKVMMachine.Status.Ready = true once the domain is running.
//   - On delete: destroy the libvirt domain, delete the disk volume,
//     remove the finalizer.
type ZKVMMachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=zkvmmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=zkvmmachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=zkvmmachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=zkvmclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch

// Reconcile is the main reconciliation loop for ZKVMMachine.
func (r *ZKVMMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("zkvmmachine", req.NamespacedName)

	// ── Fetch ZKVMMachine ─────────────────────────────────────────────────────
	var zkvmMachine infrav1.ZKVMMachine
	if err := r.Get(ctx, req.NamespacedName, &zkvmMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: get ZKVMMachine: %w", err)
	}

	// ── Fetch the owning CAPI Machine ─────────────────────────────────────────
	machine, err := util.GetOwnerMachine(ctx, r.Client, zkvmMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: get owner Machine: %w", err)
	}
	if machine == nil {
		log.Info("Owner Machine not set yet — requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// ── Fetch the owning CAPI Cluster ─────────────────────────────────────────
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: get Cluster: %w", err)
	}
	if cluster == nil {
		log.Info("Owner Cluster not set yet — requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// ── Fetch the ZKVMCluster (our infrastructure cluster object) ─────────────
	var zkvmCluster infrav1.ZKVMCluster
	zkvmClusterKey := client.ObjectKey{
		Namespace: zkvmMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Get(ctx, zkvmClusterKey, &zkvmCluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: get ZKVMCluster: %w", err)
	}

	// ── Bug 4: Gate on ZKVMCluster.Status.Ready ───────────────────────────────
	// The ZKVMCluster reconciler validates the KVM host, storage pool, and
	// network bridge before setting Ready=true. Proceeding before that point
	// would cause spurious libvirt connection failures every reconcile loop.
	if !zkvmCluster.Status.Ready {
		log.Info("ZKVMCluster not ready yet — requeuing", "zkvmCluster", zkvmClusterKey.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// ── Set up patch helper ───────────────────────────────────────────────────
	patchHelper, err := patch.NewHelper(&zkvmMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: patch helper: %w", err)
	}
	defer func() {
		if patchErr := patchHelper.Patch(ctx, &zkvmMachine); patchErr != nil {
			log.Error(patchErr, "Failed to patch ZKVMMachine")
		}
	}()

	// ── Handle deletion ───────────────────────────────────────────────────────
	if !zkvmMachine.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, log, &zkvmMachine, &zkvmCluster)
	}

	// ── Add finalizer ─────────────────────────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&zkvmMachine, zkvmMachineFinalizer) {
		controllerutil.AddFinalizer(&zkvmMachine, zkvmMachineFinalizer)
	}

	// ── Normal reconciliation ─────────────────────────────────────────────────
	return r.reconcileNormal(ctx, log, &zkvmMachine, &zkvmCluster, machine)
}

// reconcileNormal creates the libvirt disk + VM for a ZKVMMachine.
func (r *ZKVMMachineReconciler) reconcileNormal(
	ctx context.Context,
	log logr.Logger,
	zkvmMachine *infrav1.ZKVMMachine,
	zkvmCluster *infrav1.ZKVMCluster,
	machine *clusterv1.Machine,
) (ctrl.Result, error) {

	// Already done — idempotent check.
	if zkvmMachine.Status.Ready {
		return ctrl.Result{}, nil
	}

	log.Info("Reconciling ZKVMMachine — creating VM",
		"machine", zkvmMachine.Name,
		"kvmHost", zkvmCluster.Spec.KVMHost,
	)

	// Read SSH key if referenced
	var keyPath string
	if zkvmCluster.Spec.SSHKeySecretRef != nil {
		secretKey := client.ObjectKey{
			Namespace: zkvmCluster.Namespace,
			Name:      zkvmCluster.Spec.SSHKeySecretRef.Name,
		}
		var secret corev1.Secret
		if err := r.Get(ctx, secretKey, &secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: get SSHKeySecret: %w", err)
		}
		sshKey, ok := secret.Data["ssh-privatekey"]
		if !ok || len(sshKey) == 0 {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: secret %q missing key 'ssh-privatekey'", secretKey.Name)
		}
		tmpFile, err := os.CreateTemp("", "zkvm-ssh-*")
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: create temp SSH key: %w", err)
		}
		if err := os.Chmod(tmpFile.Name(), 0600); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: chmod temp SSH key: %w", err)
		}
		if _, err := tmpFile.Write(sshKey); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: write temp SSH key: %w", err)
		}
		tmpFile.Close()
		keyPath = tmpFile.Name()
		defer os.Remove(keyPath)
	}

	// Open libvirt connection with auth options
	kvm, err := zkvmlibvirt.NewClientWithAuth(zkvmlibvirt.ClientOptions{
		KVMHost: zkvmCluster.Spec.KVMHost,
		SSHUser: zkvmCluster.Spec.SSHUser,
		KeyPath: keyPath,
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: connect to KVM host: %w", err)
	}
	defer kvm.Close()

	domainName := zkvmMachine.Name
	volName := domainName + ".qcow2"

	// ── Step 1: Create disk volume (idempotent) ───────────────────────────────
	volExists, err := kvm.VolumeExists(zkvmCluster.Spec.StoragePool, volName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: VolumeExists: %w", err)
	}

	var diskPath string
	if !volExists {
		log.Info("Creating disk volume", "volume", volName, "sizeGB", zkvmMachine.Spec.DiskGB)
		diskPath, err = kvm.CreateVolume(zkvmlibvirt.VolumeCreateParams{
			PoolName: zkvmCluster.Spec.StoragePool,
			Name:     volName,
			SizeGB:   zkvmMachine.Spec.DiskGB,
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: CreateVolume: %w", err)
		}
		log.Info("Disk volume created", "path", diskPath)
	} else {
		log.Info("Disk volume already exists — fetching path", "volume", volName)
		// Bug 1: Use GetVolumePath() instead of reconstructing the path
		// from a hardcoded convention. The pool may not live under
		// /var/lib/libvirt/images/ on all deployments.
		diskPath, err = kvm.GetVolumePath(zkvmCluster.Spec.StoragePool, volName)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: GetVolumePath: %w", err)
		}
	}

	// ── Step 2: Define libvirt domain (idempotent) ────────────────────────────
	domExists, err := kvm.DomainExists(domainName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: DomainExists: %w", err)
	}

	if !domExists {
		// MAC address: derive from machine name deterministically.
		// Format: 52:54:00:XX:XX:XX — using last 3 bytes of a hash.
		mac := deriveMACAddress(domainName)

		// ABI ignition is embedded inside the initrd (agent.s390x-initrd.img).
		// Nodes do not need to fetch ignition from an external URL — the
		// rendezvous agent inside the initrd handles everything.
		// The kernel cmdline only needs rd.neednet=1 for network bring-up
		// and static IP args so the node can reach the rendezvous service.
		cmdline := "rd.neednet=1 console=ttysclp0"
		if zkvmMachine.Spec.IPAddress != "" {
			// Format static IP cmdline: ip=<client-ip>::<gw-ip>:<netmask>:<hostname>:<device>:none
			// On s390x CCW virtio, leaving the device empty (::none) lets dracut auto-bind to the primary network device (enc1).
			gw := zkvmMachine.Spec.Gateway
			mask := zkvmMachine.Spec.Netmask
			if mask == "" {
				mask = "255.255.255.0"
			}
			cmdline = fmt.Sprintf("%s ip=%s::%s:%s:%s::none", cmdline, zkvmMachine.Spec.IPAddress, gw, mask, domainName)
			if zkvmMachine.Spec.Nameserver != "" {
				cmdline = fmt.Sprintf("%s nameserver=%s", cmdline, zkvmMachine.Spec.Nameserver)
			}
		}

		log.Info("Defining libvirt domain", "domain", domainName)
		err = kvm.DefineDomain(zkvmlibvirt.DomainCreateParams{
			Name:          domainName,
			VCPUs:         zkvmMachine.Spec.VCPUs,
			MemoryGB:      zkvmMachine.Spec.MemoryGB,
			DiskPath:      diskPath,
			NetworkBridge: zkvmCluster.Spec.NetworkBridge,
			MACAddress:    mac,
			// Bug 7: Read KernelPath/InitrdPath from ZKVMClusterSpec
			// instead of hardcoding a convention that may not hold.
			KernelPath:    zkvmCluster.Spec.KernelPath,
			InitrdPath:    zkvmCluster.Spec.InitrdPath,
			KernelCmdline: cmdline,
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: DefineDomain: %w", err)
		}
		log.Info("Libvirt domain defined", "domain", domainName)
	} else {
		log.Info("Libvirt domain already defined — skipping", "domain", domainName)
	}

	// ── Step 3: Start the VM (idempotent) ────────────────────────────────────
	// Bug 3: Check domain state before calling Create() so we don't error on
	// an already-running VM. DOMAIN_RUNNING means it is up; any other state
	// (DOMAIN_SHUTOFF, DOMAIN_PAUSED, …) is safe to start.
	isRunning, err := kvm.IsDomainRunning(domainName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: IsDomainRunning: %w", err)
	}
	if !isRunning {
		if err := kvm.StartDomain(domainName); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: StartDomain: %w", err)
		}
		log.Info("VM started", "domain", domainName)
	} else {
		log.Info("VM already running — skipping start", "domain", domainName)
	}

	// ── Step 4: Set ProviderID and mark Ready ────────────────────────────────
	// With ABI bootstrap (no kubeadm), Machine.Status.NodeRef is never
	// populated by CAPI bootstrap — OCP's own node lifecycle controller
	// manages node registration independently. We mark ZKVMMachine Ready
	// as soon as the libvirt domain is confirmed running. This unblocks the
	// CAPI Cluster status reconciliation so it reports infrastructure ready.
	providerID := fmt.Sprintf("zkvm://%s/%s", zkvmCluster.Spec.KVMHost, domainName)
	zkvmMachine.Spec.ProviderID = &providerID

	// Populate Status.Addresses from the domain's static IP (from spec).
	// We use the spec IP directly since it is statically assigned — no need
	// to query libvirt DHCP leases. This is always available immediately.
	if zkvmMachine.Spec.IPAddress != "" {
		zkvmMachine.Status.Addresses = []clusterv1.MachineAddress{
			{
				Type:    clusterv1.MachineInternalIP,
				Address: zkvmMachine.Spec.IPAddress,
			},
		}
	}

	zkvmMachine.Status.Ready = true
	log.Info("ZKVMMachine ready — VM is running", "domain", domainName, "providerID", providerID)
	return ctrl.Result{}, nil
}

// reconcileDelete destroys the libvirt VM and its disk, then removes the finalizer.
func (r *ZKVMMachineReconciler) reconcileDelete(
	ctx context.Context,
	log logr.Logger,
	zkvmMachine *infrav1.ZKVMMachine,
	zkvmCluster *infrav1.ZKVMCluster,
) (ctrl.Result, error) {
	log.Info("Reconciling ZKVMMachine deletion", "machine", zkvmMachine.Name)

	// Read SSH key secret — same logic as reconcileNormal.
	// Using NewClient() here would ignore SSH key auth and fail on hosts
	// that require key-based authentication (the common case).
	var keyPath string
	if zkvmCluster.Spec.SSHKeySecretRef != nil {
		secretKey := client.ObjectKey{
			Namespace: zkvmCluster.Namespace,
			Name:      zkvmCluster.Spec.SSHKeySecretRef.Name,
		}
		var secret corev1.Secret
		if err := r.Get(ctx, secretKey, &secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler delete: get SSHKeySecret: %w", err)
		}
		sshKey, ok := secret.Data["ssh-privatekey"]
		if !ok || len(sshKey) == 0 {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler delete: secret %q missing key 'ssh-privatekey'", secretKey.Name)
		}
		tmpFile, err := os.CreateTemp("", "zkvm-ssh-*")
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler delete: create temp SSH key: %w", err)
		}
		if err := os.Chmod(tmpFile.Name(), 0600); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler delete: chmod temp SSH key: %w", err)
		}
		if _, err := tmpFile.Write(sshKey); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler delete: write temp SSH key: %w", err)
		}
		tmpFile.Close()
		keyPath = tmpFile.Name()
		defer os.Remove(keyPath)
	}

	kvm, err := zkvmlibvirt.NewClientWithAuth(zkvmlibvirt.ClientOptions{
		KVMHost: zkvmCluster.Spec.KVMHost,
		SSHUser: zkvmCluster.Spec.SSHUser,
		KeyPath: keyPath,
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler delete: connect to KVM host: %w", err)
	}
	defer kvm.Close()

	domainName := zkvmMachine.Name
	volName := domainName + ".qcow2"

	// Destroy VM (force power off + undefine).
	log.Info("Destroying libvirt domain", "domain", domainName)
	if err := kvm.DestroyDomain(domainName); err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler delete: DestroyDomain: %w", err)
	}
	log.Info("Libvirt domain destroyed", "domain", domainName)

	// Delete disk volume.
	log.Info("Deleting disk volume", "volume", volName)
	if err := kvm.DeleteVolume(zkvmCluster.Spec.StoragePool, volName); err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler delete: DeleteVolume: %w", err)
	}
	log.Info("Disk volume deleted", "volume", volName)

	// Remove finalizer — object can now be garbage collected.
	controllerutil.RemoveFinalizer(zkvmMachine, zkvmMachineFinalizer)
	log.Info("ZKVMMachine finalizer removed — object will be garbage collected")

	return ctrl.Result{}, nil
}

// SetupWithManager registers ZKVMMachineReconciler with the controller-runtime manager.
// It also watches ZKVMCluster objects so that when a ZKVMCluster transitions to Ready,
// all ZKVMMachines in the same namespace are re-enqueued immediately.
func (r *ZKVMMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ZKVMMachine{}).
		Watches(
			&infrav1.ZKVMCluster{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				machineList := &infrav1.ZKVMMachineList{}
				if err := mgr.GetClient().List(ctx, machineList, client.InNamespace(obj.GetNamespace())); err != nil {
					return nil
				}
				requests := make([]reconcile.Request, len(machineList.Items))
				for i, m := range machineList.Items {
					requests[i] = reconcile.Request{
						NamespacedName: types.NamespacedName{
							Namespace: m.Namespace,
							Name:      m.Name,
						},
					}
				}
				return requests
			}),
		).
		Complete(r)
}

// deriveMACAddress generates a deterministic MAC address from a domain name.
// Bug 8: The previous 3-byte rotating XOR had anagram collisions — different
// names with the same character multiset produced identical MACs, causing
// duplicate MAC addresses on the network.
//
// Fix: use the first 3 bytes of SHA-256(name). SHA-256 is collision-resistant
// and anagram-safe; any two distinct names produce different digests with
// overwhelming probability. The QEMU/KVM vendor prefix 52:54:00 is preserved.
func deriveMACAddress(name string) string {
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", digest[0], digest[1], digest[2])
}
