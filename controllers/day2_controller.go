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

// day2_controller.go — day-2 worker post-boot reconciliation.
//
// This file contains all logic that runs after a day-2 replacement VM is
// started. It is called from reconcileNormal when isDay2 is true and the
// VM is already running.
//
// Responsibilities:
//   - writeWorkerIgnition: fetch worker-user-data-managed from the target
//     cluster and write it to the KVM host web directory over SSH.
//   - reconcileDay2PostBoot: poll until the OCP Node appears, approve both
//     rounds of kubelet CSRs (verified by name+IP), patch spec.providerID
//     on the OCP Node, then mark ZKVMMachine.Status.Ready.
//   - buildTargetClient: build a client-go REST client from the
//     <cluster>-kubeconfig Secret on the SNO.

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/go-logr/logr"
	certsv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"golang.org/x/crypto/ssh"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/openshift-smart-installer/cluster-api-provider-zkvm/api/v1alpha1"
)

// writeWorkerIgnition fetches worker-user-data-managed from the target cluster
// and writes the content to <day2Boot.webRoot>/worker.ign on the KVM host over SSH.
// A temp file + rename is used so the VM never fetches a half-written file.
func (r *ZKVMMachineReconciler) writeWorkerIgnition(
	ctx context.Context,
	log logr.Logger,
	zkvmCluster *infrav1.ZKVMCluster,
	keyPath string,
) error {
	// Build a client for the target cluster.
	targetClient, err := r.buildTargetKubeClient(ctx, zkvmCluster)
	if err != nil {
		return fmt.Errorf("writeWorkerIgnition: build target client: %w", err)
	}

	// Fetch worker-user-data-managed Secret from the target cluster.
	secret, err := targetClient.CoreV1().Secrets("openshift-machine-api").
		Get(ctx, "worker-user-data-managed", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("writeWorkerIgnition: get worker-user-data-managed: %w", err)
	}
	ignData, ok := secret.Data["userData"]
	if !ok || len(ignData) == 0 {
		return fmt.Errorf("writeWorkerIgnition: worker-user-data-managed missing key 'userData'")
	}
	log.Info("writeWorkerIgnition: fetched worker ignition", "bytes", len(ignData))

	// Write to the KVM host web directory over SSH.
	kvmUser := zkvmCluster.Spec.SSHUser
	if kvmUser == "" {
		kvmUser = "root"
	}
	sshClient, err := dialSSHFromKey(zkvmCluster.Spec.KVMHost, kvmUser, keyPath)
	if err != nil {
		return fmt.Errorf("writeWorkerIgnition: SSH to KVM host: %w", err)
	}
	defer sshClient.Close()

	webRoot := zkvmCluster.Spec.Day2Boot.WebRoot
	tmpPath := webRoot + "/worker.ign.tmp"
	finalPath := webRoot + "/worker.ign"

	// Write to temp file.
	sess, err := sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("writeWorkerIgnition: new SSH session: %w", err)
	}
	defer sess.Close()
	sess.Stdin = bytes.NewReader(ignData)
	if out, err := sess.CombinedOutput("cat > " + tmpPath); err != nil {
		return fmt.Errorf("writeWorkerIgnition: write %s: %w — %s", tmpPath, err, string(out))
	}

	// Atomic rename.
	sess2, err := sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("writeWorkerIgnition: new SSH session for rename: %w", err)
	}
	defer sess2.Close()
	if out, err := sess2.CombinedOutput("mv " + tmpPath + " " + finalPath); err != nil {
		return fmt.Errorf("writeWorkerIgnition: rename to %s: %w — %s", finalPath, err, string(out))
	}

	log.Info("writeWorkerIgnition: worker.ign written to KVM host", "path", finalPath)
	return nil
}

// reconcileDay2PostBoot is called after the day-2 VM has been started.
// It polls every 15 s for:
//  1. Pending kubelet client CSRs matching this machine — approves them.
//  2. Pending kubelet serving CSRs matching this machine — approves them.
//  3. The OCP Node to reach Ready state.
//  4. Patches spec.providerID on the Node so CAPI sets NodeRef.
//
// Once all four are done it marks ZKVMMachine.Status.Ready = true.
func (r *ZKVMMachineReconciler) reconcileDay2PostBoot(
	ctx context.Context,
	log logr.Logger,
	zkvmMachine *infrav1.ZKVMMachine,
	zkvmCluster *infrav1.ZKVMCluster,
	domainName string,
	providerID string,
) (ctrl.Result, error) {
	targetClient, err := r.buildTargetKubeClient(ctx, zkvmCluster)
	if err != nil {
		log.Info("reconcileDay2PostBoot: target client not ready yet — requeuing", "err", err.Error())
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Approve any pending CSRs that match this machine.
	if err := r.approveMatchingCSRs(ctx, log, targetClient, domainName, zkvmMachine.Spec.IPAddress); err != nil {
		log.Error(err, "reconcileDay2PostBoot: CSR approval error — requeuing")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Check whether the Node exists and is Ready.
	node, err := targetClient.CoreV1().Nodes().Get(ctx, domainName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("reconcileDay2PostBoot: Node not yet registered — requeuing", "node", domainName)
		} else {
			log.Error(err, "reconcileDay2PostBoot: get Node error — requeuing")
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	nodeReady := false
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
			nodeReady = true
			break
		}
	}
	if !nodeReady {
		log.Info("reconcileDay2PostBoot: Node exists but not yet Ready — requeuing", "node", domainName)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Patch spec.providerID on the Node so CAPI can link the Node to its Machine.
	if node.Spec.ProviderID == "" {
		patch := fmt.Sprintf(`{"spec":{"providerID":%q}}`, providerID)
		if _, err := targetClient.CoreV1().Nodes().Patch(
			ctx, domainName,
			types.MergePatchType,
			[]byte(patch),
			metav1.PatchOptions{},
		); err != nil {
			log.Error(err, "reconcileDay2PostBoot: patch providerID failed — requeuing")
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		log.Info("reconcileDay2PostBoot: providerID patched on Node", "node", domainName, "providerID", providerID)
	}

	// All done.
	zkvmMachine.Status.Ready = true
	log.Info("ZKVMMachine ready (day-2 path) — Node Ready and providerID set",
		"domain", domainName, "providerID", providerID)
	return ctrl.Result{}, nil
}

// approveMatchingCSRs lists all pending CertificateSigningRequests on the
// target cluster and approves those that match the expected node name and IP.
//
// Two CSR types are handled differently:
//
//   - kube-apiserver-client-kubelet (client CSR, round 1):
//     Submitted by node-bootstrapper on behalf of the new node. Never contains
//     an IP SAN by design — only the node CN. Approved on CN match alone.
//
//   - kubelet-serving (serving CSR, round 2):
//     Submitted by the node itself after the client cert is issued. Contains
//     both CN and IP SANs. Approved only when CN AND IP SAN both match —
//     this prevents approving serving CSRs from rogue nodes on the same bridge.
func (r *ZKVMMachineReconciler) approveMatchingCSRs(
	ctx context.Context,
	log logr.Logger,
	targetClient kubernetes.Interface,
	domainName string,
	ipAddress string,
) error {
	csrList, err := targetClient.CertificatesV1().CertificateSigningRequests().
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("approveMatchingCSRs: list CSRs: %w", err)
	}

	expectedCN := "system:node:" + domainName
	expectedIP := net.ParseIP(ipAddress)

	for i := range csrList.Items {
		csr := &csrList.Items[i]
		// Skip already-decided CSRs.
		if csrDecided(csr) {
			continue
		}

		// Decode the CSR request bytes.
		block, _ := pem.Decode(csr.Spec.Request)
		if block == nil {
			continue
		}
		req, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			continue
		}

		// Check CN.
		if req.Subject.CommonName != expectedCN {
			continue
		}

		// For serving CSRs (kubelet-serving signer) verify IP SAN — prevents
		// approving a serving cert for a rogue node that shares the CN.
		// For client CSRs (kube-apiserver-client-kubelet signer) CN match is
		// sufficient — client CSRs never contain IP SANs by design.
		isServingCSR := csr.Spec.SignerName == "kubernetes.io/kubelet-serving"
		if isServingCSR && expectedIP != nil {
			matched := false
			for _, san := range req.IPAddresses {
				if san.Equal(expectedIP) {
					matched = true
					break
				}
			}
			if !matched {
				log.Info("approveMatchingCSRs: serving CSR — CN matches but no IP SAN match — skipping",
					"csr", csr.Name, "expectedIP", ipAddress)
				continue
			}
		}

		// Approve.
		csr.Status.Conditions = append(csr.Status.Conditions, certsv1.CertificateSigningRequestCondition{
			Type:               certsv1.CertificateApproved,
			Status:             corev1.ConditionTrue,
			Reason:             "ZKVMControllerApproved",
			Message:            "Approved by zkvm controller: CN and IP match ZKVMMachine",
			LastUpdateTime:     metav1.Now(),
		})
		if _, err := targetClient.CertificatesV1().CertificateSigningRequests().
			UpdateApproval(ctx, csr.Name, csr, metav1.UpdateOptions{}); err != nil {
			log.Error(err, "approveMatchingCSRs: UpdateApproval failed", "csr", csr.Name)
			continue
		}
		log.Info("approveMatchingCSRs: approved CSR", "csr", csr.Name, "node", domainName)
	}
	return nil
}

// csrDecided returns true if the CSR already has an Approved or Denied condition.
func csrDecided(csr *certsv1.CertificateSigningRequest) bool {
	for _, c := range csr.Status.Conditions {
		if c.Type == certsv1.CertificateApproved || c.Type == certsv1.CertificateDenied {
			return true
		}
	}
	return false
}

// buildTargetKubeClient builds a Kubernetes client for the target cluster by
// reading the <cluster>-kubeconfig Secret from the SNO (bootstrap cluster).
// The Secret is created by the installer after install-complete and carries the
// ownerReference, label, and type required by the CAPI controller informer.
func (r *ZKVMMachineReconciler) buildTargetKubeClient(
	ctx context.Context,
	zkvmCluster *infrav1.ZKVMCluster,
) (kubernetes.Interface, error) {
	secretName := zkvmCluster.Name + "-kubeconfig"
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: zkvmCluster.Namespace,
		Name:      secretName,
	}, &secret); err != nil {
		return nil, fmt.Errorf("buildTargetKubeClient: get Secret %s: %w", secretName, err)
	}

	kubeconfigBytes, ok := secret.Data["value"]
	if !ok || len(kubeconfigBytes) == 0 {
		return nil, fmt.Errorf("buildTargetKubeClient: Secret %s missing key 'value'", secretName)
	}

	restCfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("buildTargetKubeClient: parse kubeconfig: %w", err)
	}
	// Increase timeouts for the target cluster which may be slow.
	restCfg.Timeout = 10 * time.Second
	restCfg.QPS = 5
	restCfg.Burst = 10

	kc, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("buildTargetKubeClient: new kubernetes client: %w", err)
	}
	return kc, nil
}

// dialSSHFromKey opens an SSH client connection to the KVM host using the
// given private key file path. Returns the *ssh.Client directly so callers
// can open multiple sessions.
func dialSSHFromKey(host, user, keyPath string) (*ssh.Client, error) {
	if keyPath == "" {
		return nil, fmt.Errorf("dialSSHFromKey: keyPath is empty")
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("dialSSHFromKey: read key %q: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return nil, fmt.Errorf("dialSSHFromKey: parse key %q: %w", keyPath, err)
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec — internal bridge; future: pin host key
		Timeout:         30 * time.Second,
	}
	c, err := ssh.Dial("tcp", host+":22", cfg)
	if err != nil {
		return nil, fmt.Errorf("dialSSHFromKey: dial %s: %w", host, err)
	}
	return c, nil
}
