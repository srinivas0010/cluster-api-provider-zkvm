package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	infrav1 "github.com/openshift-smart-installer/cluster-api-provider-zkvm/api/v1alpha1"
)

var (
	// Scheme contains the standard K8s types, CAPI Core types, and ZKVM infrastructure types.
	Scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(Scheme))
	utilruntime.Must(clusterv1.AddToScheme(Scheme))
	utilruntime.Must(infrav1.AddToScheme(Scheme))
}

// LocalControlPlane manages the lifecycle of the in-process envtest control plane (etcd + apiserver).
type LocalControlPlane struct {
	Env            *envtest.Environment
	Client         client.Client
	Cfg            *rest.Config
	KubeconfigPath string
	WorkDir        string
	log            logr.Logger
}

// NewLocalControlPlane initializes a new local control plane instance in workDir.
func NewLocalControlPlane(workDir string, log logr.Logger) *LocalControlPlane {
	return &LocalControlPlane{
		WorkDir: workDir,
		log:     log.WithName("local-control-plane"),
	}
}

// Start boots the in-process etcd + kube-apiserver and installs the embedded CAPI & ZKVM CRDs.
func (lcp *LocalControlPlane) Start(ctx context.Context) error {
	lcp.log.Info("Starting in-process local control plane (etcd + kube-apiserver)")

	if err := os.MkdirAll(lcp.WorkDir, 0755); err != nil {
		return fmt.Errorf("create work dir %s: %w", lcp.WorkDir, err)
	}

	// 1. Extract embedded CRDs to a temporary directory so envtest can load them.
	crdDir := filepath.Join(lcp.WorkDir, "crds")
	if err := os.MkdirAll(crdDir, 0755); err != nil {
		return fmt.Errorf("create crd dir: %w", err)
	}

	entries, err := crdFS.ReadDir("crds")
	if err != nil {
		return fmt.Errorf("read embedded crds: %w", err)
	}

	for _, entry := range entries {
		data, err := crdFS.ReadFile("crds/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read embedded crd file %s: %w", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(crdDir, entry.Name()), data, 0644); err != nil {
			return fmt.Errorf("write crd file %s: %w", entry.Name(), err)
		}
	}

	// 2. Configure envtest environment.
	etcdDataDir := filepath.Join(lcp.WorkDir, "etcd")
	lcp.Env = &envtest.Environment{
		Scheme: Scheme,
		CRDDirectoryPaths: []string{crdDir},
		ErrorIfCRDPathMissing: true,
		ControlPlane: envtest.ControlPlane{
			Etcd: &envtest.Etcd{
				DataDir: etcdDataDir,
			},
		},
	}

	cfg, err := lcp.Env.Start()
	if err != nil {
		return fmt.Errorf("envtest.Start failed: %w", err)
	}
	lcp.Cfg = cfg

	// 3. Write out local kubeconfig for the in-process API server.
	kubeconfigBytes, err := generateKubeconfig(cfg)
	if err != nil {
		return fmt.Errorf("generate kubeconfig: %w", err)
	}

	lcp.KubeconfigPath = filepath.Join(lcp.WorkDir, "envtest.kubeconfig")
	if err := os.WriteFile(lcp.KubeconfigPath, kubeconfigBytes, 0600); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}

	// 4. Create controller-runtime direct client.
	k8sClient, err := client.New(cfg, client.Options{Scheme: Scheme})
	if err != nil {
		return fmt.Errorf("create k8s client: %w", err)
	}
	lcp.Client = k8sClient

	lcp.log.Info("Local control plane ready", "kubeconfig", lcp.KubeconfigPath)
	return nil
}

// Stop shuts down the in-process etcd and apiserver.
func (lcp *LocalControlPlane) Stop() error {
	if lcp.Env == nil {
		return nil
	}
	lcp.log.Info("Stopping local control plane")
	return lcp.Env.Stop()
}

// generateKubeconfig creates a serialized kubeconfig from rest.Config.
func generateKubeconfig(cfg *rest.Config) ([]byte, error) {
	clusterName := "local-bootstrap"
	contextName := "local-bootstrap"
	userName := "local-bootstrap-admin"

	config := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			clusterName: {
				Server:                   cfg.Host,
				CertificateAuthorityData: cfg.CAData,
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			contextName: {
				Cluster:  clusterName,
				AuthInfo: userName,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			userName: {
				ClientCertificateData: cfg.CertData,
				ClientKeyData:         cfg.KeyData,
			},
		},
		CurrentContext: contextName,
	}

	return clientcmd.Write(config)
}
