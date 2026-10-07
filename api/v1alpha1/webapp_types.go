// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Rijngshaffen

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ConditionAvailable is True when the desired number of replicas is serving.
	ConditionAvailable = "Available"
	// ConditionProgressing is True while a rollout or scale operation is in flight.
	ConditionProgressing = "Progressing"
	// ConditionDegraded is True when the operator failed to reconcile a child resource.
	ConditionDegraded = "Degraded"
)

// WebAppSpec defines the desired state of a WebApp.
type WebAppSpec struct {
	// Image is the container image to run, e.g. "nginx:1.29".
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Replicas is the desired number of pods.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Port is the port the container listens on.
	// +kubebuilder:default=8080
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// ServicePort is the port the Service exposes.
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	ServicePort int32 `json:"servicePort,omitempty"`

	// ServiceType is the type of the Service fronting the pods.
	// +kubebuilder:default=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +optional
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`

	// HealthPath is an HTTP path used for readiness and liveness probes.
	// When empty, readiness falls back to a TCP check on Port and no
	// liveness probe is configured.
	// +kubebuilder:validation:Pattern=`^/.*$`
	// +optional
	HealthPath string `json:"healthPath,omitempty"`

	// Env is a list of environment variables set on the container.
	// +listType=map
	// +listMapKey=name
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Resources are the compute resource requirements of the container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Ingress exposes the app outside the cluster. When omitted, no Ingress
	// is created and an existing one is removed.
	// +optional
	Ingress *IngressSpec `json:"ingress,omitempty"`
}

// IngressSpec configures the Ingress created for a WebApp.
type IngressSpec struct {
	// Host is the fully qualified domain name the app is served on.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Host string `json:"host"`

	// Path is the URL path prefix routed to the app.
	// +kubebuilder:default="/"
	// +kubebuilder:validation:Pattern=`^/.*$`
	// +optional
	Path string `json:"path,omitempty"`

	// ClassName selects the IngressClass. When omitted the cluster default is used.
	// +optional
	ClassName *string `json:"className,omitempty"`

	// TLSSecretName is the name of a kubernetes.io/tls Secret in the same
	// namespace. When set, TLS is enabled for Host.
	// +optional
	TLSSecretName string `json:"tlsSecretName,omitempty"`
}

// WebAppStatus defines the observed state of a WebApp.
type WebAppStatus struct {
	// ObservedGeneration is the most recent generation acted on by the operator.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ReadyReplicas is the number of pods that are ready to serve.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Selector is the label selector for the pods, in string form. It backs
	// the scale subresource so that HorizontalPodAutoscalers can target a WebApp.
	// +optional
	Selector string `json:"selector,omitempty"`

	// URL is where the app can be reached: the Ingress URL when one is
	// configured, otherwise the in-cluster Service URL.
	// +optional
	URL string `json:"url,omitempty"`

	// Conditions represent the latest observations of the WebApp's state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// WebApp is the Schema for the webapps API. Each WebApp is reconciled into a
// Deployment, a Service and, optionally, an Ingress.
//
// The name is reused for the Service, so it has to be a valid Service name.
//
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 63",message="metadata.name must be a DNS label: at most 63 lower case alphanumeric characters or '-', starting with a letter and ending with an alphanumeric character"
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.readyReplicas,selectorpath=.status.selector
// +kubebuilder:resource:shortName=wa,categories=all
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type WebApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WebAppSpec   `json:"spec,omitempty"`
	Status WebAppStatus `json:"status,omitempty"`
}

// WebAppList contains a list of WebApp.
//
// +kubebuilder:object:root=true
type WebAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebApp `json:"items"`
}
