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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/approval,verbs=update
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=signers,resourceNames=kubernetes.io/kubelet-serving;kubernetes.io/kube-apiserver-client-kubelet,verbs=approve

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

	// ── Already fully reconciled ─────────────────────────────────────────────
	// Status.Ready is set only after the VM is running AND (for day-2 workers)
	// CSRs are approved and providerID is patched on the OCP Node.
	// ABI control-plane and initial workers set Ready immediately after VM start
	// (see bottom of this function); day-2 workers requeue until the Node is Ready.
	if zkvmMachine.Status.Ready {
		return ctrl.Result{}, nil
	}

	// ── Step 0: Dynamic IP allocation (MachineDeployment / MachineSet path) ──
	if zkvmMachine.Spec.IPAddress == "" {
		ip, err := r.claimWorkerIP(ctx, log, zkvmMachine, zkvmCluster)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: claimWorkerIP: %w", err)
		}
		if ip == "" {
			log.Info("No IP available in WorkerIPPool — requeuing",
				"pool", zkvmCluster.Spec.WorkerIPPool)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		zkvmMachine.Spec.IPAddress = ip
		// Inherit network settings from cluster-level defaults when the
		// ZKVMMachineTemplate did not set them (e.g. old templates without gateway).
		if zkvmMachine.Spec.Nameserver == "" {
			zkvmMachine.Spec.Nameserver = zkvmCluster.Spec.BastionIP
		}
		// Gateway fallback: use GatewayIP from the cluster spec (the KVM host
		// bridge IP, e.g. 192.168.153.1). Falls back to BastionIP only if
		// GatewayIP was not populated (old clusters). Using BastionIP as gateway
		// causes MCD Pull to take up to 9 hours because the bastion does not
		// route internet traffic — the KVM host does.
		if zkvmMachine.Spec.Gateway == "" {
			if zkvmCluster.Spec.GatewayIP != "" {
				zkvmMachine.Spec.Gateway = zkvmCluster.Spec.GatewayIP
			} else {
				zkvmMachine.Spec.Gateway = zkvmCluster.Spec.BastionIP
			}
		}
		log.Info("Claimed worker IP from pool", "ip", ip, "machine", zkvmMachine.Name)
	}

	// ── Determine whether this is a day-2 worker ─────────────────────────────
	// A machine uses the day-2 RHCOS live PXE path when BOTH:
	//   1. ZKVMCluster.Spec.Day2Boot is set (installer patches this after install-complete).
	//   2. The CAPI Machine carries the deployment-name label (workers only; never control plane).
	_, hasDeploymentLabel := machine.Labels["cluster.x-k8s.io/deployment-name"]
	isDay2 := zkvmCluster.Spec.Day2Boot != nil && hasDeploymentLabel

	log.Info("Reconciling ZKVMMachine — creating VM",
		"machine", zkvmMachine.Name,
		"kvmHost", zkvmCluster.Spec.KVMHost,
		"day2", isDay2,
	)

	// ── Read SSH key ──────────────────────────────────────────────────────────
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

	// ── Day-2: write fresh worker.ign to KVM host before defining the domain ──
	// For day-2 workers: fetch worker-user-data-managed from the target cluster
	// and write it to the web-served directory so the VM can fetch it during boot.
	if isDay2 {
		if err := r.writeWorkerIgnition(ctx, log, zkvmCluster, keyPath); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: writeWorkerIgnition: %w", err)
		}
	}

	// ── Open libvirt connection ───────────────────────────────────────────────
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
		mac := deriveMACAddress(zkvmMachine.Spec.IPAddress)

		gw := zkvmMachine.Spec.Gateway
		mask := zkvmMachine.Spec.Netmask
		if mask == "" {
			mask = "255.255.255.0"
		}

		var cmdline, kernelPath, initrdPath string

		if isDay2 {
			// ── Day-2 coreos-installer cmdline ────────────────────────────────
			// Uses the generic RHCOS live PXE kernel/initrd (from Day2Boot) as
			// the installer environment, and the OCP-specific rootfs served by
			// the bastion HTTPD as the image written to disk.
			//
			// Why generic RHCOS live kernel/initrd (not ABI):
			//   The ABI kernel/initrd is the OpenShift agent-based installer.
			//   After writing to disk it issues a guest poweroff, not a reboot.
			//   On s390x/CCW a poweroff halts the CPU but libvirt still reports
			//   the domain as running — the installed OS never gets to boot.
			//   The generic RHCOS live coreos-installer issues a guest reboot
			//   instead. On s390x, guest-triggered reboots ignore the libvirt
			//   XML kernel/initrd and boot from the disk grub directly — proven
			//   by manual testing (manual-worker-22 reached Ready in ~82s).
			//
			// Why generic RHCOS rootfs (Day2Boot.RootFSURL, not ZKVMCluster.Spec.RootFSURL):
			//   coreos.live.rootfs_url drives two things:
			//     1. The live environment that coreos-installer runs in.
			//     2. The s390x partition table layout written to disk.
			//   The generic RHCOS live rootfs creates the correct GPT layout
			//   including the PReP boot partition (p1) required by zipl on s390x.
			//   ZKVMCluster.Spec.RootFSURL points at the ABI bastion rootfs —
			//   designed for agent-based install, it does not produce a PReP
			//   partition when used with standalone coreos-installer, leaving
			//   the disk without a bootloader and causing a ~40s halt on first
			//   disk boot attempt.
			//   Proven by manual test: manual-worker-22 used the generic RHCOS
			//   rootfs, got a correct PReP partition, rebooted from disk, and
			//   reached Ready. The MCO then upgrades the node to the cluster
			//   RHCOS version automatically after it joins.
			//
			// The only day-2-specific file is worker.ign, which the controller
			// writes to the KVM host web directory before starting the VM.
			// coreos-installer fetches it via coreos.inst.ignition_url and
			// writes it to disk; the installed node then contacts the MCS
			// directly using the embedded MCS URL and CA cert.
			//
			// The ip= argument is carried over by coreos-installer into the
			// installed OS as a NetworkManager keyfile — no extra network step needed.
			d2 := zkvmCluster.Spec.Day2Boot
			ignURL := d2.HTTPBaseURL + "/worker.ign"
			// rootfs: use the generic RHCOS live rootfs (Day2Boot.RootFSURL).
			// This produces the correct s390x GPT layout with PReP boot partition.
			rootfsURL := d2.RootFSURL
			cmdline = fmt.Sprintf(
				"rd.neednet=1 console=ttysclp0 random.trust_cpu=on coreos.inst.install_dev=/dev/vda"+
					" ip=%s::%s:%s:%s::none nameserver=%s"+
					" coreos.live.rootfs_url=%s coreos.inst.ignition_url=%s",
				zkvmMachine.Spec.IPAddress, gw, mask, domainName, zkvmMachine.Spec.Nameserver,
				rootfsURL, ignURL,
			)
			// kernel + initrd: use the generic RHCOS live PXE files staged by
			// the installer (Day2Boot.KernelPath/InitrdPath). These provide the
			// coreos-installer live environment which reboots the guest on
			// completion — allowing the installed disk grub to take over.
			kernelPath = d2.KernelPath
			initrdPath = d2.InitrdPath
			log.Info("Using day-2 coreos-installer cmdline (generic RHCOS live kernel, OCP rootfs)",
				"domain", domainName, "ignURL", ignURL, "rootfsURL", rootfsURL,
				"kernelPath", kernelPath, "initrdPath", initrdPath)
		} else {
			// ── ABI cmdline (control plane + initial workers) ─────────────────
			cmdline = "rd.neednet=1 ai.ip_cfg_override=1 console=ttysclp0 random.trust_cpu=on rd.luks.options=discard ignition.firstboot ignition.platform.id=metal"
			if zkvmMachine.Spec.IPAddress != "" {
				cmdline = fmt.Sprintf("%s ip=%s::%s:%s:%s::none", cmdline, zkvmMachine.Spec.IPAddress, gw, mask, domainName)
				if zkvmMachine.Spec.Nameserver != "" {
					cmdline = fmt.Sprintf("%s nameserver=%s", cmdline, zkvmMachine.Spec.Nameserver)
				}
			}
			if zkvmCluster.Spec.RootFSURL != "" {
				cmdline = fmt.Sprintf("%s coreos.live.rootfs_url=%s", cmdline, zkvmCluster.Spec.RootFSURL)
			} else if zkvmCluster.Spec.IgnitionBaseURL != "" {
				cmdline = fmt.Sprintf("%s coreos.live.rootfs_url=%s/rootfs.img", cmdline, zkvmCluster.Spec.IgnitionBaseURL)
			}
			kernelPath = zkvmCluster.Spec.KernelPath
			initrdPath = zkvmCluster.Spec.InitrdPath
		}

		log.Info("Defining libvirt domain", "domain", domainName)
		if err := kvm.DefineDomain(zkvmlibvirt.DomainCreateParams{
			Name:          domainName,
			VCPUs:         zkvmMachine.Spec.VCPUs,
			MemoryGB:      zkvmMachine.Spec.MemoryGB,
			DiskPath:      diskPath,
			NetworkBridge: zkvmCluster.Spec.NetworkBridge,
			MACAddress:    mac,
			KernelPath:    kernelPath,
			InitrdPath:    initrdPath,
			KernelCmdline: cmdline,
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMMachineReconciler: DefineDomain: %w", err)
		}
		log.Info("Libvirt domain defined", "domain", domainName)
	} else {
		log.Info("Libvirt domain already defined — skipping", "domain", domainName)
	}

	// ── Step 3: Start the VM (idempotent) ────────────────────────────────────
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

	// ── Step 4: Set ProviderID on ZKVMMachine ────────────────────────────────
	providerID := fmt.Sprintf("zkvm://%s/%s", zkvmCluster.Spec.KVMHost, domainName)
	zkvmMachine.Spec.ProviderID = &providerID
	if zkvmMachine.Spec.IPAddress != "" {
		zkvmMachine.Status.Addresses = []clusterv1.MachineAddress{
			{Type: clusterv1.MachineInternalIP, Address: zkvmMachine.Spec.IPAddress},
		}
	}

	// ── Step 5: ABI path — mark Ready immediately ────────────────────────────
	// For ABI (control plane + initial workers) there is no kubeconfig Secret
	// yet and CSR approval is not automated. Mark Ready as soon as the VM runs.
	if !isDay2 {
		zkvmMachine.Status.Ready = true
		log.Info("ZKVMMachine ready (ABI path) — VM is running", "domain", domainName, "providerID", providerID)
		return ctrl.Result{}, nil
	}

	// ── Step 6: Day-2 path — wait for Node, approve CSRs, patch providerID ───
	// The VM is running but RHCOS is still installing. Requeue every 15 s until
	// the OCP Node appears, both CSR rounds are approved, and providerID is set.
	log.Info("Day-2 VM running — polling for Node and CSRs", "domain", domainName)
	return r.reconcileDay2PostBoot(ctx, log, zkvmMachine, zkvmCluster, domainName, providerID)
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

// claimWorkerIP scans all ZKVMMachines in the cluster namespace, builds the
// set of already-claimed IPs, and returns the first IP from WorkerIPPool that
// is not yet taken. Returns ("", nil) if the pool is exhausted — caller
// should requeue.
func (r *ZKVMMachineReconciler) claimWorkerIP(
	ctx context.Context,
	log logr.Logger,
	zkvmMachine *infrav1.ZKVMMachine,
	zkvmCluster *infrav1.ZKVMCluster,
) (string, error) {
	if len(zkvmCluster.Spec.WorkerIPPool) == 0 {
		return "", fmt.Errorf("ZKVMCluster %q has an empty WorkerIPPool — "+
			"add ip_start + ip_count to compute_node_groups in inputs-zkvm.yaml",
			zkvmCluster.Name)
	}

	// List all ZKVMMachines in this namespace.
	var machineList infrav1.ZKVMMachineList
	if err := r.List(ctx, &machineList, client.InNamespace(zkvmMachine.Namespace)); err != nil {
		return "", fmt.Errorf("claimWorkerIP: list ZKVMMachines: %w", err)
	}

	// Build set of IPs already taken by other machines.
	taken := make(map[string]bool, len(machineList.Items))
	for _, m := range machineList.Items {
		if m.Name != zkvmMachine.Name && m.Spec.IPAddress != "" {
			taken[m.Spec.IPAddress] = true
		}
	}

	// Return first unclaimed IP from the pool.
	for _, ip := range zkvmCluster.Spec.WorkerIPPool {
		if !taken[ip] {
			log.Info("claimWorkerIP: selected IP from pool", "ip", ip,
				"takenCount", len(taken), "poolSize", len(zkvmCluster.Spec.WorkerIPPool))
			return ip, nil
		}
	}

	// Pool exhausted.
	log.Info("claimWorkerIP: pool exhausted",
		"poolSize", len(zkvmCluster.Spec.WorkerIPPool), "takenCount", len(taken))
	return "", nil
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
