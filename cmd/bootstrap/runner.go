package bootstrap

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-smart-installer/cluster-api-provider-zkvm/controllers"
)

// Runner manages the in-process execution of the ZKVM controllers.
type Runner struct {
	lcp    *LocalControlPlane
	log    logr.Logger
	cancel context.CancelFunc
}

// NewRunner creates a new in-process controller runner.
func NewRunner(lcp *LocalControlPlane, log logr.Logger) *Runner {
	return &Runner{
		lcp: lcp,
		log: log.WithName("bootstrap-runner"),
	}
}

// StartManager starts the controller-runtime Manager and registers ZKVM reconcilers.
func (r *Runner) StartManager(ctx context.Context) error {
	r.log.Info("Starting in-process ZKVM controller manager")

	mgrCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	mgr, err := ctrl.NewManager(r.lcp.Cfg, ctrl.Options{
		Scheme: Scheme,
		LeaderElection: false, // Single in-process instance
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	// Register ZKVMClusterReconciler
	if err := (&controllers.ZKVMClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup ZKVMClusterReconciler: %w", err)
	}

	// Register ZKVMMachineReconciler
	if err := (&controllers.ZKVMMachineReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup ZKVMMachineReconciler: %w", err)
	}

	// Run manager in background goroutine
	go func() {
		if err := mgr.Start(mgrCtx); err != nil {
			r.log.Error(err, "In-process manager stopped with error")
		}
	}()

	r.log.Info("In-process ZKVM controller manager running")
	return nil
}

// Stop shuts down the in-process manager.
func (r *Runner) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
}

// WaitForTargetAPIServer polls the target cluster API server until it responds to health requests.
func (r *Runner) WaitForTargetAPIServer(ctx context.Context, apiEndpoint string, timeout time.Duration) error {
	r.log.Info("Waiting for target OpenShift API server to become ready", "endpoint", apiEndpoint, "timeout", timeout)

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	url := fmt.Sprintf("https://%s/readyz", apiEndpoint)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				r.log.Info("Target OpenShift API server is Ready!", "endpoint", apiEndpoint)
				return nil
			}
		}

		r.log.Info("Target API server not ready yet — retrying in 30 seconds...")
		time.Sleep(30 * time.Second)
	}

	return fmt.Errorf("timed out after %v waiting for target API server %s", timeout, apiEndpoint)
}

// ApplyManifests applies a set of Kubernetes client objects to the local control plane.
func (r *Runner) ApplyObjects(ctx context.Context, objs []client.Object) error {
	for _, obj := range objs {
		r.log.Info("Creating CAPI resource in local bootstrap control plane", "kind", obj.GetObjectKind().GroupVersionKind().Kind, "name", obj.GetName())
		if err := r.lcp.Client.Create(ctx, obj); err != nil {
			return fmt.Errorf("create object %s/%s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	return nil
}

// BuildTargetClient creates a client-go Interface from target kubeconfig file.
func BuildTargetClient(kubeconfigPath string) (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("build target config: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}
