package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/openshift-smart-installer/cluster-api-provider-zkvm/api/v1alpha1"
)

// PivotManager coordinates the migration of CAPI resources from the local bootstrap control plane
// into the newly provisioned target OpenShift cluster.
type PivotManager struct {
	lcp               *LocalControlPlane
	targetKubeconfig  string
	targetRestConfig  *rest.Config
	targetClient      client.Client
	targetKubeClient  kubernetes.Interface
	targetDynClient   dynamic.Interface
	namespace         string
	log               logr.Logger
}

// NewPivotManager creates a new PivotManager instance.
func NewPivotManager(lcp *LocalControlPlane, targetKubeconfig string, namespace string, log logr.Logger) (*PivotManager, error) {
	if namespace == "" {
		namespace = "default"
	}

	targetCfg, err := clientcmd.BuildConfigFromFlags("", targetKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load target kubeconfig %s: %w", targetKubeconfig, err)
	}

	targetK8sClient, err := client.New(targetCfg, client.Options{Scheme: Scheme})
	if err != nil {
		return nil, fmt.Errorf("create target k8s client: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(targetCfg)
	if err != nil {
		return nil, fmt.Errorf("create target kube client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(targetCfg)
	if err != nil {
		return nil, fmt.Errorf("create target dynamic client: %w", err)
	}

	return &PivotManager{
		lcp:              lcp,
		targetKubeconfig: targetKubeconfig,
		targetRestConfig: targetCfg,
		targetClient:     targetK8sClient,
		targetKubeClient: kubeClient,
		targetDynClient:  dynClient,
		namespace:        namespace,
		log:              log.WithName("pivot-manager"),
	}, nil
}

// PrepareTargetCluster applies CAPI & ZKVM CRDs, provider manifests, and OpenShift SCCs on the target cluster.
func (pm *PivotManager) PrepareTargetCluster(ctx context.Context) error {
	pm.log.Info("Preparing target OpenShift cluster for self-hosted CAPI management")

	// 1. Apply embedded CRDs to target cluster
	entries, err := crdFS.ReadDir("crds")
	if err != nil {
		return fmt.Errorf("read embedded crds: %w", err)
	}

	for _, entry := range entries {
		data, err := crdFS.ReadFile("crds/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read crd %s: %w", entry.Name(), err)
		}
		if err := pm.applyYAML(ctx, data); err != nil {
			return fmt.Errorf("apply crd %s to target: %w", entry.Name(), err)
		}
	}

	// 2. Apply embedded infrastructure-components.yaml to target cluster
	if len(infraComponentsYAML) > 0 {
		pm.log.Info("Applying infrastructure-components.yaml to target cluster")
		if err := pm.applyYAML(ctx, infraComponentsYAML); err != nil {
			return fmt.Errorf("apply infrastructure-components.yaml: %w", err)
		}
	}

	// 3. Apply OpenShift Security Context Constraints (SCC) bindings
	if err := pm.grantOpenShiftSCCs(ctx); err != nil {
		pm.log.Error(err, "Warning: failed to grant OpenShift SCC bindings (may require manual grant if not on OpenShift)")
	}

	pm.log.Info("Target OpenShift cluster preparation completed successfully")
	return nil
}

// grantOpenShiftSCCs grants necessary SCC permissions for CAPI and ZKVM controllers on OpenShift.
func (pm *PivotManager) grantOpenShiftSCCs(ctx context.Context) error {
	pm.log.Info("Granting OpenShift Security Context Constraints (SCC) for CAPI controllers")

	// Create ClusterRoleBindings to system:openshift:scc:anyuid and system:openshift:scc:privileged
	sccBindings := []struct {
		name      string
		sccRole   string
		namespace string
		saName    string
	}{
		{
			name:      "capi-zkvm-anyuid",
			sccRole:   "system:openshift:scc:anyuid",
			namespace: "capi-zkvm-system",
			saName:    "cluster-api-provider-zkvm-controller-manager",
		},
		{
			name:      "capi-manager-privileged",
			sccRole:   "system:openshift:scc:privileged",
			namespace: "capi-system",
			saName:    "capi-manager",
		},
		{
			name:      "capi-bootstrap-privileged",
			sccRole:   "system:openshift:scc:privileged",
			namespace: "capi-kubeadm-bootstrap-system",
			saName:    "capi-kubeadm-bootstrap-manager",
		},
		{
			name:      "capi-control-plane-privileged",
			sccRole:   "system:openshift:scc:privileged",
			namespace: "capi-kubeadm-control-plane-system",
			saName:    "capi-kubeadm-control-plane-manager",
		},
	}

	for _, b := range sccBindings {
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: b.name,
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     b.sccRole,
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      b.saName,
					Namespace: b.namespace,
				},
			},
		}

		err := pm.targetClient.Create(ctx, crb)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			pm.log.Info("Notice: ClusterRoleBinding for SCC not applied", "binding", b.name, "err", err.Error())
		}
	}

	return nil
}

// MoveResources transfers all CAPI resources from the local bootstrap control plane to the target cluster.
func (pm *PivotManager) MoveResources(ctx context.Context) error {
	pm.log.Info("Starting pivot: transferring CAPI resources from local bootstrap to target OpenShift cluster")

	// Resources to move in order: Secrets -> ZKVMCluster -> Cluster -> Machines -> ZKVMMachines -> MachineDeployment
	gvkList := []schema.GroupVersionResource{
		{Group: "", Version: "v1", Resource: "secrets"},
		{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Resource: "zkvmclusters"},
		{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Resource: "zkvmmachinetemplates"},
		{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "clusters"},
		{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "machines"},
		{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Resource: "zkvmmachines"},
		{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "machinedeployments"},
		{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "machinesets"},
		{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "machinehealthchecks"},
		{Group: "bootstrap.cluster.x-k8s.io", Version: "v1beta1", Resource: "kubeadmconfigtemplates"},
		{Group: "bootstrap.cluster.x-k8s.io", Version: "v1beta1", Resource: "kubeadmconfigs"},
	}

	localDynClient, err := dynamic.NewForConfig(pm.lcp.Cfg)
	if err != nil {
		return fmt.Errorf("create local dynamic client: %w", err)
	}

	var movedObjects []unstructured.Unstructured

	for _, gvr := range gvkList {
		list, err := localDynClient.Resource(gvr).Namespace(pm.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}

		for _, item := range list.Items {
			// Skip service account tokens and system secrets
			if item.GetKind() == "Secret" {
				secType, _, _ := unstructured.NestedString(item.Object, "type")
				if secType == "kubernetes.io/service-account-token" {
					continue
				}
			}

			pm.log.Info("Transferring resource to target cluster", "kind", item.GetKind(), "name", item.GetName())

			cleanItem := item.DeepCopy()
			cleanItem.SetResourceVersion("")
			cleanItem.SetUID("")
			cleanItem.SetCreationTimestamp(metav1.Time{})
			cleanItem.SetSelfLink("")
			unstructured.RemoveNestedField(cleanItem.Object, "status")

			_, err := pm.targetDynClient.Resource(gvr).Namespace(pm.namespace).Create(ctx, cleanItem, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create %s/%s on target: %w", item.GetKind(), item.GetName(), err)
			}

			movedObjects = append(movedObjects, item)
		}
	}

	pm.log.Info("All CAPI resources created on target cluster. Cleaning up local bootstrap copies...")
	for _, item := range movedObjects {
		gvr := schema.GroupVersionResource{
			Group:    item.GroupVersionKind().Group,
			Version:  item.GroupVersionKind().Version,
			Resource: getResourcePlural(item.GetKind()),
		}
		_ = localDynClient.Resource(gvr).Namespace(pm.namespace).Delete(ctx, item.GetName(), metav1.DeleteOptions{})
	}

	pm.log.Info("Pivot completed successfully. Target OpenShift cluster is now self-hosted!")
	return nil
}

// applyYAML decodes and applies a multi-document YAML stream to the target cluster.
func (pm *PivotManager) applyYAML(ctx context.Context, data []byte) error {
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var u unstructured.Unstructured
		if err := decoder.Decode(&u); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if len(u.Object) == 0 {
			continue
		}

		gvk := u.GroupVersionKind()
		mappingGVR := schema.GroupVersionResource{
			Group:    gvk.Group,
			Version:  gvk.Version,
			Resource: getResourcePlural(gvk.Kind),
		}

		var client dynamic.ResourceInterface
		if u.GetNamespace() != "" {
			client = pm.targetDynClient.Resource(mappingGVR).Namespace(u.GetNamespace())
		} else {
			client = pm.targetDynClient.Resource(mappingGVR)
		}

		_, err := client.Create(ctx, &u, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			// Try update if already exists
			_, updateErr := client.Update(ctx, &u, metav1.UpdateOptions{})
			if updateErr != nil {
				return fmt.Errorf("apply %s/%s: %w", gvk.Kind, u.GetName(), err)
			}
		}
	}
	return nil
}

func getResourcePlural(kind string) string {
	switch kind {
	case "Cluster":
		return "clusters"
	case "ZKVMCluster":
		return "zkvmclusters"
	case "Machine":
		return "machines"
	case "ZKVMMachine":
		return "zkvmmachines"
	case "ZKVMMachineTemplate":
		return "zkvmmachinetemplates"
	case "MachineDeployment":
		return "machinedeployments"
	case "MachineSet":
		return "machinesets"
	case "MachineHealthCheck":
		return "machinehealthchecks"
	case "KubeadmConfigTemplate":
		return "kubeadmconfigtemplates"
	case "KubeadmConfig":
		return "kubeadmconfigs"
	case "CustomResourceDefinition":
		return "customresourcedefinitions"
	case "Secret":
		return "secrets"
	case "Namespace":
		return "namespaces"
	case "ServiceAccount":
		return "serviceaccounts"
	case "ClusterRole":
		return "clusterroles"
	case "ClusterRoleBinding":
		return "clusterrolebindings"
	case "Role":
		return "roles"
	case "RoleBinding":
		return "rolebindings"
	case "Deployment":
		return "deployments"
	case "Service":
		return "services"
	default:
		return strings.ToLower(kind) + "s"
	}
}
