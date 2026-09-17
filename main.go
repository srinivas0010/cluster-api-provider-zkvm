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

// main.go is the entry point for the cluster-api-provider-zkvm controller manager.
//
// It:
//   1. Sets up the controller-runtime manager with the zkvm scheme registered.
//   2. Registers ZKVMClusterReconciler and ZKVMMachineReconciler with the manager.
//   3. Starts the manager — which runs the reconcilers and the health/metrics servers.
//
// The manager is configured to watch all namespaces by default.
// Use the --namespace flag to restrict to a single namespace if needed.
package main

import (
	"flag"
	"os"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	infrav1 "github.com/openshift-smart-installer/cluster-api-provider-zkvm/api/v1alpha1"
	"github.com/openshift-smart-installer/cluster-api-provider-zkvm/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	// Register all types the controller needs to work with.
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))  // core k8s types (Pod, Secret, etc.)
	utilruntime.Must(clusterv1.AddToScheme(scheme))        // CAPI core types (Cluster, Machine, etc.)
	utilruntime.Must(infrav1.AddToScheme(scheme))          // our zkvm types (ZKVMCluster, ZKVMMachine, etc.)
}

func main() {
	// ── CLI flags ─────────────────────────────────────────────────────────────
	var (
		metricsAddr          string
		enableLeaderElection bool
		probeAddr            string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")

	// Zap logger options — level and encoding configurable at runtime.
	opts := zap.Options{
		Development: false,
		TimeEncoder: zapcore.ISO8601TimeEncoder,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// ── Create the manager ────────────────────────────────────────────────────
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "zkvm.infrastructure.cluster.x-k8s.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to create controller manager")
		os.Exit(1)
	}

	// ── Register ZKVMClusterReconciler ────────────────────────────────────────
	if err := (&controllers.ZKVMClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Log:    ctrl.Log.WithName("controllers").WithName("ZKVMCluster"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create ZKVMCluster controller")
		os.Exit(1)
	}

	// ── Register ZKVMMachineReconciler ────────────────────────────────────────
	if err := (&controllers.ZKVMMachineReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Log:    ctrl.Log.WithName("controllers").WithName("ZKVMMachine"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create ZKVMMachine controller")
		os.Exit(1)
	}

	// ── Register admission webhooks ───────────────────────────────────────────
	if err := (&infrav1.ZKVMCluster{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create ZKVMCluster webhook")
		os.Exit(1)
	}
	if err := (&infrav1.ZKVMMachine{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create ZKVMMachine webhook")
		os.Exit(1)
	}

	// ── Health checks ─────────────────────────────────────────────────────────
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// ── Start the manager (blocks until signal or error) ─────────────────────
	setupLog.Info("Starting cluster-api-provider-zkvm manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
