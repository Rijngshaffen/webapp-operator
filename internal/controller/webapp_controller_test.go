// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Rijngshaffen

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	webappv1alpha1 "github.com/Rijngshaffen/webapp-operator/api/v1alpha1"
)

const (
	timeout  = 10 * time.Second
	interval = 100 * time.Millisecond
)

var ctx = context.Background()

func newWebApp(namespace, name string) *webappv1alpha1.WebApp {
	return &webappv1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       webappv1alpha1.WebAppSpec{Image: "nginx:1.29"},
	}
}

// eventually retries check until it passes. Assertions inside check must use
// the Gomega passed to it.
func eventually(t *testing.T, check func(g Gomega)) {
	t.Helper()
	NewWithT(t).Eventually(check, timeout, interval).Should(Succeed())
}

// updateWebApp applies mutate to the latest version of app, retrying on
// conflicts with the controller's own writes.
func updateWebApp(t *testing.T, app *webappv1alpha1.WebApp, mutate func()) {
	t.Helper()
	eventually(t, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(app), app)).To(Succeed())
		mutate()
		g.Expect(k8sClient.Update(ctx, app)).To(Succeed())
	})
}

// markDeploymentReady plays the role of the Deployment controller and kubelet,
// which do not exist in envtest.
func markDeploymentReady(t *testing.T, key client.ObjectKey) {
	t.Helper()
	eventually(t, func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		n := *dep.Spec.Replicas
		dep.Status = appsv1.DeploymentStatus{
			ObservedGeneration: dep.Generation,
			Replicas:           n,
			UpdatedReplicas:    n,
			ReadyReplicas:      n,
			AvailableReplicas:  n,
			Conditions: []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentAvailable,
				Status: corev1.ConditionTrue,
				Reason: "MinimumReplicasAvailable",
			}},
		}
		g.Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
	})
}

func conditionStatus(app *webappv1alpha1.WebApp, condType string) metav1.ConditionStatus {
	if c := meta.FindStatusCondition(app.Status.Conditions, condType); c != nil {
		return c.Status
	}
	return ""
}

func TestCreatesDeploymentAndService(t *testing.T) {
	g := NewWithT(t)
	app := newWebApp(newNamespace(t), "hello")
	g.Expect(k8sClient.Create(ctx, app)).To(Succeed())
	key := client.ObjectKeyFromObject(app)

	// CRD defaults are applied by the API server.
	g.Expect(app.Spec.Replicas).To(HaveValue(BeEquivalentTo(1)))
	g.Expect(app.Spec.Port).To(BeEquivalentTo(8080))
	g.Expect(app.Spec.ServicePort).To(BeEquivalentTo(80))
	g.Expect(app.Spec.ServiceType).To(Equal(corev1.ServiceTypeClusterIP))

	eventually(t, func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		g.Expect(dep.Spec.Replicas).To(HaveValue(BeEquivalentTo(1)))
		g.Expect(dep.Spec.Selector.MatchLabels).To(Equal(selectorLabels(app)))
		g.Expect(metav1.IsControlledBy(dep, app)).To(BeTrue())

		g.Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
		c := dep.Spec.Template.Spec.Containers[0]
		g.Expect(c.Image).To(Equal("nginx:1.29"))
		g.Expect(c.Ports).To(ConsistOf(HaveField("ContainerPort", BeEquivalentTo(8080))))
		g.Expect(c.ReadinessProbe.TCPSocket).NotTo(BeNil())
		g.Expect(c.LivenessProbe).To(BeNil())
	})

	eventually(t, func(g Gomega) {
		svc := &corev1.Service{}
		g.Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
		g.Expect(svc.Spec.Selector).To(Equal(selectorLabels(app)))
		g.Expect(svc.Spec.Ports).To(HaveLen(1))
		g.Expect(svc.Spec.Ports[0].Port).To(BeEquivalentTo(80))
		g.Expect(svc.Spec.Ports[0].TargetPort).To(Equal(intstr.FromString("http")))
		g.Expect(metav1.IsControlledBy(svc, app)).To(BeTrue())
	})

	g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &networkingv1.Ingress{}))).To(BeTrue(),
		"no Ingress should exist when spec.ingress is unset")

	// No pods can become ready in envtest, so the app is rolling out forever.
	eventually(t, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		g.Expect(app.Finalizers).To(ContainElement(Finalizer))
		g.Expect(app.Status.ObservedGeneration).To(Equal(app.Generation))
		g.Expect(app.Status.URL).To(Equal("http://hello." + app.Namespace + ".svc:80"))
		g.Expect(app.Status.Selector).To(Equal("app.kubernetes.io/instance=hello,app.kubernetes.io/name=webapp"))
		g.Expect(conditionStatus(app, webappv1alpha1.ConditionAvailable)).To(Equal(metav1.ConditionFalse))
		g.Expect(conditionStatus(app, webappv1alpha1.ConditionProgressing)).To(Equal(metav1.ConditionTrue))
		g.Expect(conditionStatus(app, webappv1alpha1.ConditionDegraded)).To(Equal(metav1.ConditionFalse))
	})
}

func TestStatusFollowsDeployment(t *testing.T) {
	g := NewWithT(t)
	app := newWebApp(newNamespace(t), "status")
	app.Spec.Replicas = ptr.To[int32](3)
	g.Expect(k8sClient.Create(ctx, app)).To(Succeed())
	key := client.ObjectKeyFromObject(app)

	markDeploymentReady(t, key)
	eventually(t, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		g.Expect(app.Status.ReadyReplicas).To(BeEquivalentTo(3))
		g.Expect(conditionStatus(app, webappv1alpha1.ConditionAvailable)).To(Equal(metav1.ConditionTrue))
		g.Expect(conditionStatus(app, webappv1alpha1.ConditionProgressing)).To(Equal(metav1.ConditionFalse))
	})

	// A new image starts a rollout: still available, but progressing again
	// until the Deployment controller reports the new generation as done.
	updateWebApp(t, app, func() { app.Spec.Image = "nginx:1.30" })
	eventually(t, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		g.Expect(app.Status.ObservedGeneration).To(Equal(app.Generation))
		g.Expect(conditionStatus(app, webappv1alpha1.ConditionAvailable)).To(Equal(metav1.ConditionTrue))
		g.Expect(conditionStatus(app, webappv1alpha1.ConditionProgressing)).To(Equal(metav1.ConditionTrue))
	})

	updateWebApp(t, app, func() { app.Spec.Replicas = ptr.To[int32](0) })
	markDeploymentReady(t, key)
	eventually(t, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		c := meta.FindStatusCondition(app.Status.Conditions, webappv1alpha1.ConditionAvailable)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(c.Reason).To(Equal("ScaledToZero"))
	})
}

func TestSpecChangesPropagate(t *testing.T) {
	g := NewWithT(t)
	app := newWebApp(newNamespace(t), "update")
	app.Spec.ServiceType = corev1.ServiceTypeNodePort
	g.Expect(k8sClient.Create(ctx, app)).To(Succeed())
	key := client.ObjectKeyFromObject(app)

	eventually(t, func(g Gomega) {
		svc := &corev1.Service{}
		g.Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
		g.Expect(svc.Spec.Ports[0].NodePort).NotTo(BeZero())
	})

	updateWebApp(t, app, func() {
		app.Spec.Image = "nginx:1.30"
		app.Spec.Port = 9090
		app.Spec.ServicePort = 8000
		app.Spec.ServiceType = corev1.ServiceTypeClusterIP
		app.Spec.HealthPath = "/healthz"
		app.Spec.Env = []corev1.EnvVar{{Name: "MODE", Value: "prod"}}
	})

	eventually(t, func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		c := dep.Spec.Template.Spec.Containers[0]
		g.Expect(c.Image).To(Equal("nginx:1.30"))
		g.Expect(c.Ports[0].ContainerPort).To(BeEquivalentTo(9090))
		g.Expect(c.Env).To(ConsistOf(corev1.EnvVar{Name: "MODE", Value: "prod"}))
		g.Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/healthz"))
		g.Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/healthz"))

		svc := &corev1.Service{}
		g.Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
		g.Expect(svc.Spec.Ports[0].Port).To(BeEquivalentTo(8000))
		g.Expect(svc.Spec.Ports[0].NodePort).To(BeZero())
	})
}

// The scale subresource is what `kubectl scale` and HorizontalPodAutoscalers use.
func TestScaleSubresource(t *testing.T) {
	g := NewWithT(t)
	app := newWebApp(newNamespace(t), "scale")
	g.Expect(k8sClient.Create(ctx, app)).To(Succeed())
	key := client.ObjectKeyFromObject(app)

	scale := &autoscalingv1.Scale{Spec: autoscalingv1.ScaleSpec{Replicas: 5}}
	g.Expect(k8sClient.SubResource("scale").Update(ctx, app, client.WithSubResourceBody(scale))).To(Succeed())

	eventually(t, func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		g.Expect(dep.Spec.Replicas).To(HaveValue(BeEquivalentTo(5)))
	})

	markDeploymentReady(t, key)
	eventually(t, func(g Gomega) {
		g.Expect(k8sClient.SubResource("scale").Get(ctx, app, scale)).To(Succeed())
		g.Expect(scale.Status.Replicas).To(BeEquivalentTo(5))
		g.Expect(scale.Status.Selector).To(Equal("app.kubernetes.io/instance=scale,app.kubernetes.io/name=webapp"))
	})
}

func TestIngressLifecycle(t *testing.T) {
	g := NewWithT(t)
	app := newWebApp(newNamespace(t), "web")
	app.Spec.Ingress = &webappv1alpha1.IngressSpec{
		Host:      "web.example.com",
		ClassName: ptr.To("nginx"),
	}
	g.Expect(k8sClient.Create(ctx, app)).To(Succeed())
	key := client.ObjectKeyFromObject(app)

	eventually(t, func(g Gomega) {
		ing := &networkingv1.Ingress{}
		g.Expect(k8sClient.Get(ctx, key, ing)).To(Succeed())
		g.Expect(metav1.IsControlledBy(ing, app)).To(BeTrue())
		g.Expect(ing.Spec.IngressClassName).To(HaveValue(Equal("nginx")))
		g.Expect(ing.Spec.TLS).To(BeEmpty())
		g.Expect(ing.Spec.Rules).To(HaveLen(1))
		g.Expect(ing.Spec.Rules[0].Host).To(Equal("web.example.com"))
		path := ing.Spec.Rules[0].HTTP.Paths[0]
		g.Expect(path.Path).To(Equal("/"))
		g.Expect(path.Backend.Service.Name).To(Equal("web"))
		g.Expect(path.Backend.Service.Port.Name).To(Equal("http"))

		g.Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		g.Expect(app.Status.URL).To(Equal("http://web.example.com/"))
	})

	updateWebApp(t, app, func() { app.Spec.Ingress.TLSSecretName = "web-tls" })
	eventually(t, func(g Gomega) {
		ing := &networkingv1.Ingress{}
		g.Expect(k8sClient.Get(ctx, key, ing)).To(Succeed())
		g.Expect(ing.Spec.TLS).To(ConsistOf(networkingv1.IngressTLS{
			Hosts:      []string{"web.example.com"},
			SecretName: "web-tls",
		}))
		g.Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		g.Expect(app.Status.URL).To(Equal("https://web.example.com/"))
	})

	updateWebApp(t, app, func() { app.Spec.Ingress = nil })
	eventually(t, func(g Gomega) {
		g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &networkingv1.Ingress{}))).To(BeTrue())
		g.Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		g.Expect(app.Status.URL).To(Equal("http://web." + app.Namespace + ".svc:80"))
	})
}

func TestRevertsDrift(t *testing.T) {
	g := NewWithT(t)
	app := newWebApp(newNamespace(t), "heal")
	app.Spec.Replicas = ptr.To[int32](2)
	g.Expect(k8sClient.Create(ctx, app)).To(Succeed())
	key := client.ObjectKeyFromObject(app)

	svc := &corev1.Service{}
	eventually(t, func(g Gomega) { g.Expect(k8sClient.Get(ctx, key, svc)).To(Succeed()) })
	originalUID := svc.UID
	g.Expect(k8sClient.Delete(ctx, svc)).To(Succeed())
	eventually(t, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		g.Expect(svc.UID).NotTo(Equal(originalUID), "the Service should have been recreated")
	})

	eventually(t, func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Spec.Replicas = ptr.To[int32](7)
		dep.Spec.Template.Spec.Containers[0].Image = "evil:latest"
		g.Expect(k8sClient.Update(ctx, dep)).To(Succeed())
	})
	eventually(t, func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		g.Expect(dep.Spec.Replicas).To(HaveValue(BeEquivalentTo(2)))
		g.Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.29"))
	})
}

// Once in sync, the operator must stop writing. A spec that the API server
// rewrites on admission (defaulted fields) is the classic cause of an operator
// that updates its children in a hot loop.
func TestSettlesWithoutRewriting(t *testing.T) {
	g := NewWithT(t)
	app := newWebApp(newNamespace(t), "settle")
	app.Spec.HealthPath = "/"
	app.Spec.Env = []corev1.EnvVar{{
		Name: "POD_NAME",
		// The API server defaults fieldRef.apiVersion to "v1".
		ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
	}}
	// The API server defaults requests to the limits.
	app.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}
	app.Spec.Ingress = &webappv1alpha1.IngressSpec{Host: "settle.example.com"}
	g.Expect(k8sClient.Create(ctx, app)).To(Succeed())
	key := client.ObjectKeyFromObject(app)

	markDeploymentReady(t, key)
	eventually(t, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		g.Expect(conditionStatus(app, webappv1alpha1.ConditionProgressing)).To(Equal(metav1.ConditionFalse))
	})

	versions := func() []string {
		var out []string
		for _, obj := range []client.Object{app, &appsv1.Deployment{}, &corev1.Service{}, &networkingv1.Ingress{}} {
			g.Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
			out = append(out, obj.GetResourceVersion())
		}
		return out
	}
	settled := versions()
	g.Consistently(versions, 2*time.Second, interval).Should(Equal(settled))

	// Nothing was changed after creation, so nothing may be reported as updated.
	events := &eventsv1.EventList{}
	g.Expect(k8sClient.List(ctx, events, client.InNamespace(app.Namespace))).To(Succeed())
	g.Expect(events.Items).NotTo(BeEmpty())
	for _, e := range events.Items {
		g.Expect(e.Note).NotTo(ContainSubstring("updated"))
	}
}

func TestDeletionTakesAppOutOfRotation(t *testing.T) {
	g := NewWithT(t)
	app := newWebApp(newNamespace(t), "bye")
	app.Spec.Ingress = &webappv1alpha1.IngressSpec{Host: "bye.example.com"}
	g.Expect(k8sClient.Create(ctx, app)).To(Succeed())
	key := client.ObjectKeyFromObject(app)

	eventually(t, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, &appsv1.Deployment{})).To(Succeed())
		g.Expect(k8sClient.Get(ctx, key, &corev1.Service{})).To(Succeed())
		g.Expect(k8sClient.Get(ctx, key, &networkingv1.Ingress{})).To(Succeed())
	})

	g.Expect(k8sClient.Delete(ctx, app)).To(Succeed())
	eventually(t, func(g Gomega) {
		g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &networkingv1.Ingress{}))).To(BeTrue())
		g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &corev1.Service{}))).To(BeTrue())
		g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, app))).To(BeTrue(), "the finalizer should have been released")
	})

	// envtest runs no garbage collector, so the Deployment is still there; in
	// a real cluster its owner reference gets it deleted.
	dep := &appsv1.Deployment{}
	g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
	g.Expect(dep.OwnerReferences).To(ConsistOf(HaveField("UID", app.UID)))
}

// A pre-existing object that belongs to someone else must be left alone, and
// the conflict must be visible on the WebApp.
func TestDoesNotTakeOverForeignObjects(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t)

	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "someone-else", Namespace: ns}}
	g.Expect(k8sClient.Create(ctx, owner)).To(Succeed())
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "taken",
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       owner.Name,
				UID:        owner.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "other"},
			Ports:    []corev1.ServicePort{{Port: 1234}},
		},
	}
	g.Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

	app := newWebApp(ns, "taken")
	g.Expect(k8sClient.Create(ctx, app)).To(Succeed())
	key := client.ObjectKeyFromObject(app)

	eventually(t, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		c := meta.FindStatusCondition(app.Status.Conditions, webappv1alpha1.ConditionDegraded)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(c.Reason).To(Equal("ReconcileFailed"))
		g.Expect(c.Message).To(ContainSubstring("Service"))
	})

	svc := &corev1.Service{}
	g.Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{"app": "other"}))
	g.Expect(svc.Spec.Ports[0].Port).To(BeEquivalentTo(1234))

	// Deleting the WebApp must not delete the Service it never owned.
	g.Expect(k8sClient.Delete(ctx, app)).To(Succeed())
	eventually(t, func(g Gomega) {
		g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, app))).To(BeTrue())
	})
	g.Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
}

// An object with the WebApp's name that nobody controls is not ours either.
// It is invisible to the label-filtered cache, so this also covers the path
// that looks it up directly in the API server.
func TestDoesNotAdoptUnownedObjects(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t)

	unowned := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "squatter", Namespace: ns},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "other"},
			Ports:    []corev1.ServicePort{{Port: 1234}},
		},
	}
	g.Expect(k8sClient.Create(ctx, unowned)).To(Succeed())

	app := newWebApp(ns, "squatter")
	g.Expect(k8sClient.Create(ctx, app)).To(Succeed())
	key := client.ObjectKeyFromObject(app)

	eventually(t, func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		c := meta.FindStatusCondition(app.Status.Conditions, webappv1alpha1.ConditionDegraded)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(c.Message).To(ContainSubstring(`Service "squatter" already exists and is not controlled by this WebApp`))
	})

	svc := &corev1.Service{}
	g.Consistently(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		g.Expect(svc.OwnerReferences).To(BeEmpty())
		g.Expect(svc.Labels).To(BeEmpty())
		g.Expect(svc.Spec.Selector).To(Equal(map[string]string{"app": "other"}))
	}, time.Second, interval).Should(Succeed())

	g.Expect(k8sClient.Delete(ctx, app)).To(Succeed())
	eventually(t, func(g Gomega) {
		g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, app))).To(BeTrue())
	})
	g.Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
}

// A child that loses the managed-by label drops out of the operator's cache.
// The operator must notice, put the label back and keep managing the object
// rather than fail on a create that can never succeed.
func TestRestoresManagedByLabel(t *testing.T) {
	g := NewWithT(t)
	app := newWebApp(newNamespace(t), "relabel")
	g.Expect(k8sClient.Create(ctx, app)).To(Succeed())
	key := client.ObjectKeyFromObject(app)

	eventually(t, func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		delete(dep.Labels, ManagedByLabel)
		g.Expect(k8sClient.Update(ctx, dep)).To(Succeed())
	})

	eventually(t, func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		g.Expect(dep.Labels).To(HaveKeyWithValue(ManagedByLabel, ManagedByValue))
	})

	// Still managed: a spec change reaches the Deployment.
	updateWebApp(t, app, func() { app.Spec.Image = "nginx:1.30" })
	eventually(t, func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		g.Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.30"))
		g.Expect(k8sClient.Get(ctx, key, app)).To(Succeed())
		g.Expect(conditionStatus(app, webappv1alpha1.ConditionDegraded)).To(Equal(metav1.ConditionFalse))
	})
}

func TestCRDValidation(t *testing.T) {
	ns := newNamespace(t)
	for name, mutate := range map[string]func(*webappv1alpha1.WebApp){
		"empty image":          func(a *webappv1alpha1.WebApp) { a.Spec.Image = "" },
		"negative replicas":    func(a *webappv1alpha1.WebApp) { a.Spec.Replicas = ptr.To[int32](-1) },
		"port out of range":    func(a *webappv1alpha1.WebApp) { a.Spec.Port = 70000 },
		"unknown service type": func(a *webappv1alpha1.WebApp) { a.Spec.ServiceType = "ExternalName" },
		"relative health path": func(a *webappv1alpha1.WebApp) { a.Spec.HealthPath = "healthz" },
		"ingress without host": func(a *webappv1alpha1.WebApp) { a.Spec.Ingress = &webappv1alpha1.IngressSpec{} },
		// Valid as an object name, but not as the name of the Service.
		"name starting with a digit": func(a *webappv1alpha1.WebApp) { a.Name = "1app" },
		"name with a dot":            func(a *webappv1alpha1.WebApp) { a.Name = "my.app" },
		"name longer than 63":        func(a *webappv1alpha1.WebApp) { a.Name = strings.Repeat("a", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			app := newWebApp(ns, "invalid")
			mutate(app)
			err := k8sClient.Create(ctx, app)
			NewWithT(t).Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		})
	}
}

func TestSetStatusProgressDeadlineExceeded(t *testing.T) {
	g := NewWithT(t)
	app := newWebApp("default", "stuck")
	app.Spec.Replicas = ptr.To[int32](2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 3,
			Replicas:           3,
			UpdatedReplicas:    1,
			ReadyReplicas:      2,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
				{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "timed out"},
			},
		},
	}

	setStatus(app, dep, nil)

	c := meta.FindStatusCondition(app.Status.Conditions, webappv1alpha1.ConditionProgressing)
	g.Expect(c).NotTo(BeNil())
	g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(c.Reason).To(Equal("ProgressDeadlineExceeded"))
	g.Expect(c.Message).To(Equal("timed out"))
	g.Expect(conditionStatus(app, webappv1alpha1.ConditionAvailable)).To(Equal(metav1.ConditionTrue))
}
