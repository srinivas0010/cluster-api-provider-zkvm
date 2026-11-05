package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	version = "v0.1.0"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Printf("zkvm-installer version: %s (s390x)\n", version)
		return
	case "create":
		runCreate(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command %q\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`zkvm-installer — Standalone in-process CAPI installer for IBM Z KVM

Usage:
  zkvm-installer create cluster [options]
  zkvm-installer version

Options:
  --manifest string           Path to CAPI cluster manifest YAML (e.g. capi-cluster-demo.yaml)
  --target-kubeconfig string  Path where target cluster kubeconfig will be found/saved
  --work-dir string           Working directory for in-process local control plane (default: /tmp/zkvm-bootstrap)
  --timeout duration          Timeout for cluster provisioning (default: 45m)
  --skip-pivot                Provision cluster on KVM host without pivoting CAPI state`)
}

func runCreate(args []string) {
	fs := flag.NewFlagSet("create cluster", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "Path to CAPI cluster manifest YAML")
	targetKubeconfig := fs.String("target-kubeconfig", "", "Path to target cluster kubeconfig")
	workDir := fs.String("work-dir", "/tmp/zkvm-bootstrap", "Working directory for in-process control plane")
	timeout := fs.Duration("timeout", 45*time.Minute, "Timeout for cluster provisioning")
	skipPivot := fs.Bool("skip-pivot", false, "Skip self-hosted pivot to target cluster")

	_ = fs.Parse(args)

	if *manifestPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --manifest is required")
		fs.Usage()
		os.Exit(1)
	}

	// Setup Zap structured logger
	zapCfg := zap.NewDevelopmentConfig()
	zapCfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	zapLog, err := zapCfg.Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	log := zapr.NewLogger(zapLog)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Handle graceful shutdown signals (SIGINT, SIGTERM)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// Step 1: Start In-Process Local Control Plane (envtest)
	log.Info("=== Step 1: Initializing in-process local control plane (envtest) ===")
	lcp := NewLocalControlPlane(*workDir, log)
	if err := lcp.Start(ctx); err != nil {
		log.Error(err, "Failed to start local control plane")
		os.Exit(1)
	}
	defer lcp.Stop()

	// Step 2: Start In-Process ZKVM Controller Manager
	log.Info("=== Step 2: Starting in-process ZKVM controller manager ===")
	runner := NewRunner(lcp, log)
	if err := runner.StartManager(ctx); err != nil {
		log.Error(err, "Failed to start in-process controller manager")
		os.Exit(1)
	}
	defer runner.Stop()

	// Step 3: Parse and Apply Manifest Objects
	log.Info("=== Step 3: Applying CAPI cluster manifest ===", "manifest", *manifestPath)
	manifestData, err := os.ReadFile(*manifestPath)
	if err != nil {
		log.Error(err, "Failed to read manifest file")
		os.Exit(1)
	}

	objs, apiEndpoint, err := parseManifestObjects(manifestData)
	if err != nil {
		log.Error(err, "Failed to parse manifest objects")
		os.Exit(1)
	}

	if err := runner.ApplyObjects(ctx, objs); err != nil {
		log.Error(err, "Failed to apply objects to local control plane")
		os.Exit(1)
	}

	log.Info("CAPI cluster objects applied. ZKVM controller is provisioning VMs on KVM host...")

	// Step 4: Wait for Target OpenShift Control Plane Ready
	if apiEndpoint == "" {
		apiEndpoint = "api.capi-cluster-demo.cp.testing:6443"
	}

	log.Info("=== Step 4: Monitoring target cluster installation ===", "apiEndpoint", apiEndpoint)
	if err := runner.WaitForTargetAPIServer(ctx, apiEndpoint, *timeout); err != nil {
		log.Error(err, "Target cluster failed to become ready")
		os.Exit(1)
	}

	log.Info("Target OpenShift cluster control plane is active and responding!")

	if *skipPivot {
		log.Info("Skip-pivot flag set — keeping local control plane active until interrupted.")
		<-sigCh
		return
	}

	// Step 5: Execute Self-Hosted Pivot
	if *targetKubeconfig == "" {
		*targetKubeconfig = filepath.Join(filepath.Dir(*manifestPath), "auth", "kubeconfig")
	}

	log.Info("=== Step 5: Executing Self-Hosted Pivot to target OpenShift cluster ===", "kubeconfig", *targetKubeconfig)
	pivotMgr, err := NewPivotManager(lcp, *targetKubeconfig, "default", log)
	if err != nil {
		log.Error(err, "Failed to create PivotManager")
		os.Exit(1)
	}

	if err := pivotMgr.PrepareTargetCluster(ctx); err != nil {
		log.Error(err, "Failed to prepare target cluster with CAPI components")
		os.Exit(1)
	}

	if err := pivotMgr.MoveResources(ctx); err != nil {
		log.Error(err, "Failed to migrate CAPI resources to target cluster")
		os.Exit(1)
	}

	log.Info("=========================================================================")
	log.Info("✓ SUCCESS: OpenShift cluster is installed, pivoted, and self-hosted!")
	log.Info("=========================================================================")
}

// parseManifestObjects parses a YAML stream into client.Objects and extracts the controlPlaneEndpoint if present.
func parseManifestObjects(data []byte) ([]client.Object, string, error) {
	var objs []client.Object
	var endpoint string

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var u unstructured.Unstructured
		if err := decoder.Decode(&u); err != nil {
			if err == io.EOF {
				break
			}
			return nil, "", err
		}
		if len(u.Object) == 0 {
			continue
		}

		// Extract controlPlaneEndpoint from ZKVMCluster or Cluster spec if available
		if u.GetKind() == "ZKVMCluster" || u.GetKind() == "Cluster" {
			host, foundHost, _ := unstructured.NestedString(u.Object, "spec", "controlPlaneEndpoint", "host")
			port, foundPort, _ := unstructured.NestedInt64(u.Object, "spec", "controlPlaneEndpoint", "port")
			if foundHost && foundPort {
				endpoint = fmt.Sprintf("%s:%d", host, port)
			}
		}

		objs = append(objs, &u)
	}

	return objs, endpoint, nil
}
