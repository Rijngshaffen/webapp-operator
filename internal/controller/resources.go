// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Rijngshaffen

package controller

import (
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	webappv1alpha1 "github.com/Rijngshaffen/webapp-operator/api/v1alpha1"
)

const (
	containerName = "app"
	portName      = "http"

	// ManagedByLabel is set on every object the operator creates. The
	// operator's cache holds only objects that carry it (see CacheOptions).
	ManagedByLabel = "app.kubernetes.io/managed-by"
	// ManagedByValue is the value of ManagedByLabel.
	ManagedByValue = "webapp-operator"
)

// CacheOptions restricts the manager's cache to the child objects the operator
// manages. Without it every Deployment, Service and Ingress in the cluster
// would be held in memory, however few of them belong to a WebApp.
func CacheOptions() cache.Options {
	managed := cache.ByObject{
		Label: labels.SelectorFromSet(labels.Set{ManagedByLabel: ManagedByValue}),
	}
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&appsv1.Deployment{}:    managed,
			&corev1.Service{}:       managed,
			&networkingv1.Ingress{}: managed,
		},
		// managedFields are never read and can make up much of an object.
		DefaultTransform: cache.TransformStripManagedFields(),
	}
}

// The mutate* functions below are used with controllerutil.CreateOrUpdate.
// They set only the fields the operator owns and leave everything else on the
// live object untouched, so values defaulted by the API server (or added by
// other controllers) do not show up as drift on every reconcile.

// selectorLabels identify the pods of a WebApp. They end up in the Deployment
// selector, which is immutable, so they must never change for a given WebApp.
func selectorLabels(app *webappv1alpha1.WebApp) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "webapp",
		"app.kubernetes.io/instance": app.Name,
	}
}

func commonLabels(app *webappv1alpha1.WebApp) map[string]string {
	l := selectorLabels(app)
	l[ManagedByLabel] = ManagedByValue
	return l
}

func withLabels(existing, desired map[string]string) map[string]string {
	if existing == nil {
		existing = make(map[string]string, len(desired))
	}
	maps.Copy(existing, desired)
	return existing
}

func mutateDeployment(app *webappv1alpha1.WebApp, dep *appsv1.Deployment) {
	dep.Labels = withLabels(dep.Labels, commonLabels(app))
	dep.Spec.Replicas = app.Spec.Replicas
	if dep.CreationTimestamp.IsZero() {
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(app)}
	}

	tmpl := &dep.Spec.Template
	tmpl.Labels = withLabels(tmpl.Labels, commonLabels(app))
	if len(tmpl.Spec.Containers) == 0 {
		tmpl.Spec.Containers = []corev1.Container{{}}
	}
	c := &tmpl.Spec.Containers[0]
	c.Name = containerName
	c.Image = app.Spec.Image
	c.Env = app.Spec.Env
	c.Resources = *app.Spec.Resources.DeepCopy()
	c.Ports = []corev1.ContainerPort{{
		Name:          portName,
		ContainerPort: app.Spec.Port,
		Protocol:      corev1.ProtocolTCP,
	}}
	c.ReadinessProbe, c.LivenessProbe = probes(app)
}

// probes spells out the API server's probe defaults explicitly so that the
// desired state compares equal to the live object.
func probes(app *webappv1alpha1.WebApp) (readiness, liveness *corev1.Probe) {
	newProbe := func(h corev1.ProbeHandler) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:     h,
			TimeoutSeconds:   1,
			PeriodSeconds:    10,
			SuccessThreshold: 1,
			FailureThreshold: 3,
		}
	}
	port := intstr.FromString(portName)

	if app.Spec.HealthPath == "" {
		return newProbe(corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: port}}), nil
	}
	httpGet := func() corev1.ProbeHandler {
		return corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path:   app.Spec.HealthPath,
			Port:   port,
			Scheme: corev1.URISchemeHTTP,
		}}
	}
	return newProbe(httpGet()), newProbe(httpGet())
}

func mutateService(app *webappv1alpha1.WebApp, svc *corev1.Service) {
	svc.Labels = withLabels(svc.Labels, commonLabels(app))
	svc.Spec.Type = app.Spec.ServiceType
	svc.Spec.Selector = selectorLabels(app)

	// Reuse the existing port entry so an allocated nodePort is preserved.
	if len(svc.Spec.Ports) != 1 {
		svc.Spec.Ports = []corev1.ServicePort{{}}
	}
	p := &svc.Spec.Ports[0]
	p.Name = portName
	p.Protocol = corev1.ProtocolTCP
	p.Port = app.Spec.ServicePort
	p.TargetPort = intstr.FromString(portName)
}

func mutateIngress(app *webappv1alpha1.WebApp, ing *networkingv1.Ingress) {
	spec := app.Spec.Ingress
	pathType := networkingv1.PathTypePrefix

	ing.Labels = withLabels(ing.Labels, commonLabels(app))
	// Only set the class when asked to: with it unset, the cluster's default
	// IngressClass is filled in on admission and must not be reverted.
	if spec.ClassName != nil {
		ing.Spec.IngressClassName = spec.ClassName
	}
	ing.Spec.Rules = []networkingv1.IngressRule{{
		Host: spec.Host,
		IngressRuleValue: networkingv1.IngressRuleValue{
			HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path:     spec.Path,
					PathType: &pathType,
					Backend: networkingv1.IngressBackend{
						Service: &networkingv1.IngressServiceBackend{
							Name: app.Name,
							Port: networkingv1.ServiceBackendPort{Name: portName},
						},
					},
				}},
			},
		},
	}}
	ing.Spec.TLS = nil
	if spec.TLSSecretName != "" {
		ing.Spec.TLS = []networkingv1.IngressTLS{{
			Hosts:      []string{spec.Host},
			SecretName: spec.TLSSecretName,
		}}
	}
}

// appURL returns the Ingress URL when one is configured, otherwise the
// in-cluster Service URL.
func appURL(app *webappv1alpha1.WebApp) string {
	if ing := app.Spec.Ingress; ing != nil {
		scheme := "http"
		if ing.TLSSecretName != "" {
			scheme = "https"
		}
		return fmt.Sprintf("%s://%s%s", scheme, ing.Host, ing.Path)
	}
	return fmt.Sprintf("http://%s.%s.svc:%d", app.Name, app.Namespace, app.Spec.ServicePort)
}
