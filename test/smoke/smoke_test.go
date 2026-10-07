//go:build smoke

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Rijngshaffen

// Package smoke checks that the things that actually get shipped work
// together: the manager binary built from ./cmd and the manifests rendered
// into dist/install.yaml. Run it with `make test-smoke`.
package smoke

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	webappv1alpha1 "github.com/Rijngshaffen/webapp-operator/api/v1alpha1"
)

const (
	operatorNamespace = "webapp-operator-system"
	operatorSA        = "webapp-operator-controller-manager"
	metricsReaderRole = "webapp-operator-metrics-reader"
	leaseName         = "webapp-operator.apps.rijngshaffen.github.io"

	timeout  = 30 * time.Second
	interval = 200 * time.Millisecond
)

func TestInstallManifestAndBinary(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	root := filepath.Join("..", "..")

	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() { _ = testEnv.Stop() })

	scheme := runtime.NewScheme()
	g.Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	g.Expect(webappv1alpha1.AddToScheme(scheme)).To(Succeed())
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())

	// Every object in the install manifest must be accepted by a real API server.
	manifest, err := os.Open(filepath.Join(root, "dist", "install.yaml"))
	g.Expect(err).NotTo(HaveOccurred(), "run `make build-installer` first")
	defer func() { _ = manifest.Close() }()
	decoder := utilyaml.NewYAMLOrJSONDecoder(manifest, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); errors.Is(err, io.EOF) {
			break
		} else {
			g.Expect(err).NotTo(HaveOccurred())
		}
		if len(obj.Object) == 0 {
			continue
		}
		g.Expect(c.Create(ctx, obj)).To(Succeed(), "creating %s %s", obj.GetKind(), obj.GetName())
	}

	// Run the binary with the identity of the operator's ServiceAccount, so it
	// has exactly the permissions the manifest grants it.
	user, err := testEnv.AddUser(envtest.User{
		Name:   "system:serviceaccount:" + operatorNamespace + ":" + operatorSA,
		Groups: []string{"system:serviceaccounts", "system:authenticated"},
	}, nil)
	g.Expect(err).NotTo(HaveOccurred())
	kubeconfig, err := user.KubeConfig()
	g.Expect(err).NotTo(HaveOccurred())
	tmp := t.TempDir()
	kubeconfigPath := filepath.Join(tmp, "kubeconfig")
	g.Expect(os.WriteFile(kubeconfigPath, kubeconfig, 0o600)).To(Succeed())

	binary := filepath.Join(tmp, "manager")
	build := exec.Command("go", "build", "-o", binary, "./cmd")
	build.Dir = root
	out, err := build.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), string(out))

	probeAddr, metricsAddr := freeAddr(t), freeAddr(t)
	var logs bytes.Buffer
	manager := exec.Command(binary,
		"--kubeconfig="+kubeconfigPath,
		"--leader-elect",
		"--leader-election-namespace="+operatorNamespace,
		"--health-probe-bind-address="+probeAddr,
		"--metrics-bind-address="+metricsAddr,
	)
	manager.Stdout, manager.Stderr = &logs, &logs
	g.Expect(manager.Start()).To(Succeed())
	exited := make(chan error, 1)
	go func() { exited <- manager.Wait() }()
	t.Cleanup(func() {
		_ = manager.Process.Kill()
		if t.Failed() {
			t.Logf("manager output:\n%s", logs.String())
		}
	})

	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		// The metrics endpoint serves a self-signed certificate.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	status := func(url, token string) (int, error) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return 0, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode, nil
	}

	g.Eventually(func() (int, error) { return status("http://"+probeAddr+"/readyz", "") }, timeout, interval).
		Should(Equal(http.StatusOK))

	// Leader election works with the namespaced Role from the manifest.
	g.Eventually(func(g Gomega) {
		lease := &coordinationv1.Lease{}
		g.Expect(c.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: leaseName}, lease)).To(Succeed())
		g.Expect(lease.Spec.HolderIdentity).To(HaveValue(Not(BeEmpty())))
	}, timeout, interval).Should(Succeed())

	// The controller reconciles a WebApp.
	app := &webappv1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke", Namespace: "default"},
		Spec:       webappv1alpha1.WebAppSpec{Image: "nginx:1.29"},
	}
	g.Expect(c.Create(ctx, app)).To(Succeed())
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKeyFromObject(app), &appsv1.Deployment{})
	}, timeout, interval).Should(Succeed())

	// Metrics are only served to authenticated callers that may GET /metrics.
	metricsURL := "https://" + metricsAddr + "/metrics"
	g.Expect(status(metricsURL, "")).To(Equal(http.StatusUnauthorized))

	token := func(name string) string {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		g.Expect(c.Create(ctx, sa)).To(Succeed())
		req := &authenticationv1.TokenRequest{}
		g.Expect(c.SubResource("token").Create(ctx, sa, req)).To(Succeed())
		return req.Status.Token
	}
	g.Expect(status(metricsURL, token("stranger"))).To(Equal(http.StatusForbidden))

	scraper := token("scraper")
	g.Expect(c.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "scraper-metrics-reader"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: metricsReaderRole},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "scraper", Namespace: "default"}},
	})).To(Succeed())
	g.Expect(status(metricsURL, scraper)).To(Equal(http.StatusOK))

	// A clean shutdown on SIGTERM, as sent by the kubelet.
	g.Expect(manager.Process.Signal(syscall.SIGTERM)).To(Succeed())
	select {
	case err := <-exited:
		g.Expect(err).NotTo(HaveOccurred(), "manager should exit with status 0")
	case <-time.After(timeout):
		t.Fatal("manager did not exit after SIGTERM")
	}
	g.Expect(logs.String()).NotTo(ContainSubstring("forbidden"))
}

// freeAddr returns a loopback address with a port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return fmt.Sprintf("127.0.0.1:%d", l.Addr().(*net.TCPAddr).Port)
}
