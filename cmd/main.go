// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Rijngshaffen

// Command manager runs the WebApp operator.
package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	webappv1alpha1 "github.com/Rijngshaffen/webapp-operator/api/v1alpha1"
	"github.com/Rijngshaffen/webapp-operator/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	// version is set at build time with -ldflags "-X main.version=...".
	version = "dev"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(webappv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr    string
		probeAddr      string
		secureMetrics  bool
		leaderElection bool
		leaderElectNS  string
		concurrency    int
		printVersion   bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443",
		"The address the metrics endpoint binds to. Use 0 to disable it.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"Serve metrics over HTTPS and require Kubernetes authentication and authorization.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElection, "leader-elect", false,
		"Enable leader election so that only one replica of the operator is active.")
	flag.StringVar(&leaderElectNS, "leader-election-namespace", "",
		"The namespace of the leader election Lease. Defaults to the operator's own namespace when running in a cluster.")
	flag.IntVar(&concurrency, "max-concurrent-reconciles", 1,
		"The number of WebApps reconciled in parallel.")
	flag.BoolVar(&printVersion, "version", false, "Print the version and exit.")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	if printVersion {
		fmt.Println(version)
		return
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	metricsOpts := metricsserver.Options{BindAddress: metricsAddr, SecureServing: secureMetrics}
	if secureMetrics {
		// Callers need a ServiceAccount token that is allowed to GET /metrics;
		// see config/rbac/metrics_reader_role.yaml.
		metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Cache:                   controller.CacheOptions(),
		Metrics:                 metricsOpts,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElection,
		LeaderElectionID:        "webapp-operator.apps.rijngshaffen.github.io",
		LeaderElectionNamespace: leaderElectNS,
		// Give up the lease on shutdown so a replacement pod takes over at
		// once instead of waiting for the lease to expire. This is safe
		// because main returns as soon as the manager has stopped.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to create manager")
		os.Exit(1)
	}

	if err := (&controller.WebAppReconciler{
		Client:                  mgr.GetClient(),
		APIReader:               mgr.GetAPIReader(),
		Scheme:                  mgr.GetScheme(),
		Recorder:                mgr.GetEventRecorder("webapp-controller"),
		MaxConcurrentReconciles: concurrency,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to set up controller", "controller", "WebApp")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager", "version", version)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Manager exited with an error")
		os.Exit(1)
	}
}
