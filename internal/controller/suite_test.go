// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Rijngshaffen

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	webappv1alpha1 "github.com/Rijngshaffen/webapp-operator/api/v1alpha1"
)

// k8sClient talks directly to the envtest API server (no cache), so tests
// always observe what the controller actually wrote.
var k8sClient client.Client

// TestMain starts a real kube-apiserver and etcd (envtest), installs the CRD
// and runs the controller against them through a manager, with exactly the
// RBAC permissions it gets in production. There are no kubelet or built-in
// controllers: pods never start, Deployment status is whatever the tests
// write, and nothing is garbage collected.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set: run these tests with `make test`")
		return 1
	}
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting envtest:", err)
		return 1
	}
	defer func() { _ = testEnv.Stop() }()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := webappv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating client:", err)
		return 1
	}

	mgrCfg, err := restrictedConfig(testEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setting up RBAC:", err)
		return 1
	}
	mgr, err := ctrl.NewManager(mgrCfg, ctrl.Options{
		Scheme:  scheme,
		Cache:   CacheOptions(),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating manager:", err)
		return 1
	}
	if err := (&WebAppReconciler{
		Client:                  mgr.GetClient(),
		APIReader:               mgr.GetAPIReader(),
		Scheme:                  mgr.GetScheme(),
		Recorder:                mgr.GetEventRecorder("webapp-controller"),
		MaxConcurrentReconciles: 4,
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintln(os.Stderr, "setting up controller:", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()

	code := m.Run()

	cancel()
	if err := <-done; err != nil {
		fmt.Fprintln(os.Stderr, "manager exited with an error:", err)
		return 1
	}
	return code
}

// restrictedConfig returns credentials for a user whose only permissions are
// those of the ClusterRole shipped in config/rbac/role.yaml. Running the
// controller with them means a permission missing from the +kubebuilder:rbac
// markers fails the tests instead of surfacing in a real cluster.
func restrictedConfig(testEnv *envtest.Environment) (*rest.Config, error) {
	const userName = "webapp-operator"

	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		return nil, err
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.UnmarshalStrict(raw, role); err != nil {
		return nil, fmt.Errorf("parsing role.yaml: %w", err)
	}
	if err := k8sClient.Create(context.Background(), role); err != nil {
		return nil, err
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: userName}},
	}
	if err := k8sClient.Create(context.Background(), binding); err != nil {
		return nil, err
	}

	user, err := testEnv.AddUser(envtest.User{Name: userName}, nil)
	if err != nil {
		return nil, err
	}
	return user.Config(), nil
}

// newNamespace creates a uniquely named namespace so tests cannot interfere
// with each other.
func newNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	return ns.Name
}
