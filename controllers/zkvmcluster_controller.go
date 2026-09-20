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

// Package controllers contains the reconcilers for the zkvm CAPI provider.
//
// ZKVMClusterReconciler — watches ZKVMCluster objects; validates the KVM host
// environment (libvirtd reachable, storage pool exists, bridge exists) and
// marks the ZKVMCluster Ready once all checks pass.
//
// ZKVMMachineReconciler — watches ZKVMMachine objects; creates a libvirt VM
// when a ZKVMMachine is created and destroys it when the ZKVMMachine is deleted.
package controllers

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/openshift-smart-installer/cluster-api-provider-zkvm/api/v1alpha1"
	zkvmlibvirt "github.com/openshift-smart-installer/cluster-api-provider-zkvm/internal/libvirt"
)

const zkvmClusterFinalizer = "zkvmcluster.infrastructure.cluster.x-k8s.io"

// ZKVMClusterReconciler reconciles ZKVMCluster objects.
//
// Responsibilities:
//   - On create/update: connect to KVM host, validate storage pool + bridge,
//     set ZKVMCluster.Status.Ready = true.
//   - On delete: remove the finalizer (no infrastructure to tear down at the
//     cluster level — VMs are managed by ZKVMMachineReconciler).
type ZKVMClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=zkvmclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=zkvmclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=zkvmclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch

// Reconcile is the main reconciliation loop for ZKVMCluster.
//
// It is called by controller-runtime whenever a ZKVMCluster object is
// created, updated, or deleted, or when the controller restarts.
func (r *ZKVMClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("zkvmcluster", req.NamespacedName)

	// ── Fetch the ZKVMCluster object ─────────────────────────────────────────
	var zkvmCluster infrav1.ZKVMCluster
	if err := r.Get(ctx, req.NamespacedName, &zkvmCluster); err != nil {
		if apierrors.IsNotFound(err) {
			// Object deleted before we could reconcile — nothing to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("ZKVMClusterReconciler: get ZKVMCluster: %w", err)
	}

	// ── Fetch the owning CAPI Cluster ─────────────────────────────────────────
	cluster, err := util.GetOwnerCluster(ctx, r.Client, zkvmCluster.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMClusterReconciler: get owner Cluster: %w", err)
	}
	if cluster == nil {
		// Owner Cluster not set yet — requeue and wait.
		log.Info("Owner Cluster not found yet — requeuing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// ── Set up patch helper so we can patch status at the end ─────────────────
	patchHelper, err := patch.NewHelper(&zkvmCluster, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMClusterReconciler: patch helper: %w", err)
	}
	defer func() {
		if patchErr := patchHelper.Patch(ctx, &zkvmCluster); patchErr != nil {
			log.Error(patchErr, "Failed to patch ZKVMCluster")
		}
	}()

	// ── Handle deletion ───────────────────────────────────────────────────────
	if !zkvmCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, log, &zkvmCluster)
	}

	// ── Add finalizer if not present ──────────────────────────────────────────
	if !controllerutil.ContainsFinalizer(&zkvmCluster, zkvmClusterFinalizer) {
		controllerutil.AddFinalizer(&zkvmCluster, zkvmClusterFinalizer)
	}

	// ── Normal reconciliation: validate KVM host ──────────────────────────────
	return r.reconcileNormal(ctx, log, &zkvmCluster)
}

// reconcileNormal validates the KVM host environment and marks the cluster Ready.
func (r *ZKVMClusterReconciler) reconcileNormal(
	ctx context.Context,
	log logr.Logger,
	zkvmCluster *infrav1.ZKVMCluster,
) (ctrl.Result, error) {
	log.Info("Reconciling ZKVMCluster", "kvmHost", zkvmCluster.Spec.KVMHost)

	// Read SSH key if referenced
	var keyPath string
	if zkvmCluster.Spec.SSHKeySecretRef != nil {
		secretKey := client.ObjectKey{
			Namespace: zkvmCluster.Namespace,
			Name:      zkvmCluster.Spec.SSHKeySecretRef.Name,
		}
		var secret corev1.Secret
		if err := r.Get(ctx, secretKey, &secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMClusterReconciler: get SSHKeySecret: %w", err)
		}
		sshKey, ok := secret.Data["ssh-privatekey"]
		if !ok || len(sshKey) == 0 {
			return ctrl.Result{}, fmt.Errorf("ZKVMClusterReconciler: secret %q missing key 'ssh-privatekey'", secretKey.Name)
		}
		tmpFile, err := os.CreateTemp("", "zkvm-ssh-*")
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMClusterReconciler: create temp SSH key: %w", err)
		}
		if err := os.Chmod(tmpFile.Name(), 0600); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMClusterReconciler: chmod temp SSH key: %w", err)
		}
		if _, err := tmpFile.Write(sshKey); err != nil {
			return ctrl.Result{}, fmt.Errorf("ZKVMClusterReconciler: write temp SSH key: %w", err)
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
		msg := fmt.Sprintf("cannot connect to KVM host %s: %v", zkvmCluster.Spec.KVMHost, err)
		zkvmCluster.Status.Ready = false
		zkvmCluster.Status.FailureMessage = &msg
		log.Error(err, "Cannot connect to KVM host")
		return ctrl.Result{}, err
	}
	defer kvm.Close()

	// Validate storage pool.
	poolExists, err := kvm.StoragePoolExists(zkvmCluster.Spec.StoragePool)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMClusterReconciler: StoragePoolExists: %w", err)
	}
	if !poolExists {
		msg := fmt.Sprintf("storage pool %q not found or not active on %s", zkvmCluster.Spec.StoragePool, zkvmCluster.Spec.KVMHost)
		zkvmCluster.Status.Ready = false
		zkvmCluster.Status.FailureMessage = &msg
		log.Info("Storage pool not found", "pool", zkvmCluster.Spec.StoragePool)
		return ctrl.Result{}, fmt.Errorf("%s", msg)
	}

	// Validate network bridge.
	bridgeExists, err := kvm.BridgeExists(zkvmCluster.Spec.NetworkBridge)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ZKVMClusterReconciler: BridgeExists: %w", err)
	}
	if !bridgeExists {
		msg := fmt.Sprintf("network bridge %q not found on %s", zkvmCluster.Spec.NetworkBridge, zkvmCluster.Spec.KVMHost)
		zkvmCluster.Status.Ready = false
		zkvmCluster.Status.FailureMessage = &msg
		log.Info("Network bridge not found", "bridge", zkvmCluster.Spec.NetworkBridge)
		return ctrl.Result{}, fmt.Errorf("%s", msg)
	}

	// All checks passed — mark Ready.
	zkvmCluster.Status.Ready = true
	zkvmCluster.Status.FailureMessage = nil
	zkvmCluster.Status.FailureReason = nil
	log.Info("ZKVMCluster validated and marked Ready",
		"kvmHost", zkvmCluster.Spec.KVMHost,
		"storagePool", zkvmCluster.Spec.StoragePool,
		"bridge", zkvmCluster.Spec.NetworkBridge,
	)

	return ctrl.Result{}, nil
}

// reconcileDelete removes the finalizer so the ZKVMCluster can be garbage collected.
// No infrastructure teardown needed here — VMs are torn down by ZKVMMachineReconciler.
func (r *ZKVMClusterReconciler) reconcileDelete(
	_ context.Context,
	log logr.Logger,
	zkvmCluster *infrav1.ZKVMCluster,
) (ctrl.Result, error) {
	log.Info("Reconciling ZKVMCluster deletion")
	controllerutil.RemoveFinalizer(zkvmCluster, zkvmClusterFinalizer)
	log.Info("ZKVMCluster finalizer removed — object will be garbage collected")
	return ctrl.Result{}, nil
}

// SetupWithManager registers ZKVMClusterReconciler with the controller-runtime manager.
func (r *ZKVMClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.ZKVMCluster{}).
		Complete(r)
}
