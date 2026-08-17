/*
Copyright 2026 Nubulus Network.

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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Condition types set on a Tunnel.
const (
	// TunnelConditionSynced is whether the tunnel exists in the platform and
	// its credential is in the Secret. It is about the API, not about traffic.
	TunnelConditionSynced = "Synced"

	// TunnelConditionAgentAvailable is whether the agent Deployment has a pod
	// running. It says the client end was started, not that it connected.
	TunnelConditionAgentAvailable = "AgentAvailable"

	// TunnelConditionReady is the roll-up: synced, agent available, and the
	// platform reporting the tunnel as connected.
	TunnelConditionReady = "Ready"
)

// TunnelFinalizer keeps the object around until the tunnel has been deleted in
// the platform. Without it a `kubectl delete` would leave a tunnel holding an
// address from the account's pool that nothing in the cluster refers to any
// more.
const TunnelFinalizer = "tunnel.nubulusnetwork.es/finalizer"

// CredentialsReference points at the Secret holding an application token.
//
// The Secret is read from the Tunnel's own namespace and from nowhere else.
// That is the whole tenancy model of this operator: a namespace brings its own
// credential, and therefore its own account, so one team cannot route traffic
// through another team's account by referring to a Secret it cannot read.
type CredentialsReference struct {
	// name of the Secret in this namespace holding the application token.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// clientIDKey is the key holding the client id.
	// +kubebuilder:default="client_id"
	// +optional
	ClientIDKey string `json:"clientIDKey,omitempty"`

	// clientSecretKey is the key holding the client secret.
	// +kubebuilder:default="client_secret"
	// +optional
	ClientSecretKey string `json:"clientSecretKey,omitempty"`
}

// AgentSpec is how the tunnel client is run in this cluster.
//
// There is no replica count and there will not be one. A tunnel is a single
// WireGuard peer holding one address, so a second agent using the same
// credential fights the first for it rather than sharing the load. Running two
// means creating two Tunnel objects.
type AgentSpec struct {
	// image of the tunnel agent. Defaults to the one this operator was built
	// against, which is what makes an upgrade of the operator also an upgrade
	// of every agent it manages.
	// +optional
	Image string `json:"image,omitempty"`

	// imagePullPolicy for the agent container.
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// imagePullSecrets for a private registry.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// resources for the agent container. The agent is a small Go proxy; it
	// needs far less than most people's default.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// logLevel of the agent.
	// +kubebuilder:validation:Enum=debug;info;warn;error
	// +kubebuilder:default=info
	// +optional
	LogLevel string `json:"logLevel,omitempty"`

	// nodeSelector for the agent pod.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations for the agent pod.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// TunnelSpec is the desired state of a Tunnel.
//
// Almost nothing about a tunnel is chosen here, and that is the platform's
// design rather than an omission: the address, the key pair and the credential
// are all assigned when it is created. What this spec decides is which account
// it belongs to and how the client end is run.
type TunnelSpec struct {
	// credentials is the application token this tunnel is created with.
	Credentials CredentialsReference `json:"credentials"`

	// displayName is a label carried by the tunnel in the platform. It is
	// cosmetic: nothing keys on it, and it does not have to be unique.
	//
	// It is NOT what identifies this tunnel to the operator. That is the UID of
	// this object, sent as the tunnel's external id, which is what lets a
	// create whose answer was lost be recovered instead of repeated.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// agent is how the tunnel client is deployed in this cluster.
	// +optional
	Agent AgentSpec `json:"agent,omitempty"`

	// endpoints overrides where the operator talks to the platform. Leave it
	// unset unless you are pointing at something other than production.
	// +optional
	Endpoints *EndpointsSpec `json:"endpoints,omitempty"`
}

// EndpointsSpec overrides the platform addresses.
type EndpointsSpec struct {
	// tokenURL is the OAuth2 token endpoint.
	// +optional
	TokenURL string `json:"tokenURL,omitempty"`

	// api is the base URL of the tunnel API.
	// +optional
	API string `json:"api,omitempty"`

	// projectID is the project the access token is scoped to. It is part of
	// two of the four scopes the credential must be requested with, so it
	// changes together with the identity provider and never on its own.
	// +optional
	ProjectID string `json:"projectID,omitempty"`
}

// TunnelStatus is the observed state of a Tunnel.
type TunnelStatus struct {
	// tunnelID is the identifier the platform gave this tunnel. It is the
	// anchor for everything else, and it is recoverable from the external id
	// if it is ever lost.
	// +optional
	TunnelID string `json:"tunnelID,omitempty"`

	// externalID is what the operator sent as this tunnel's identifier, which
	// is the UID of this object. It is surfaced so that a tunnel in the panel
	// can be traced back to the object that owns it.
	// +optional
	ExternalID string `json:"externalID,omitempty"`

	// subdomain is the name the platform serves this tunnel on.
	// +optional
	Subdomain string `json:"subdomain,omitempty"`

	// cnameTarget is what a customer hostname must be pointed at for traffic to
	// arrive. Publishing it is the one step this operator cannot do: the DNS it
	// would have to write is usually somewhere else entirely.
	// +optional
	CNAMETarget string `json:"cnameTarget,omitempty"`

	// wireGuardIP is the address assigned to the client end.
	// +optional
	WireGuardIP string `json:"wireGuardIP,omitempty"`

	// credentialSecret is the Secret this tunnel's token was written to. It is
	// owned by this object and goes away with it.
	// +optional
	CredentialSecret string `json:"credentialSecret,omitempty"`

	// onlineStatus is whether the platform is currently seeing the client end,
	// derived from the last WireGuard handshake. It changes on its own with
	// nothing applied, so it is the one field here that is not a consequence of
	// the spec.
	// +optional
	OnlineStatus string `json:"onlineStatus,omitempty"`

	// observedGeneration is the generation of the spec this status was
	// computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions of the tunnel.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=tun
// +kubebuilder:printcolumn:name="Online",type=string,JSONPath=`.status.onlineStatus`
// +kubebuilder:printcolumn:name="CNAME target",type=string,JSONPath=`.status.cnameTarget`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tunnel is one WireGuard tunnel between this cluster and the platform, plus
// the agent that keeps it up.
//
// Traffic reaches the services in this cluster through the routes that point at
// it; a Tunnel on its own carries nothing.
type Tunnel struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state.
	// +required
	Spec TunnelSpec `json:"spec"`

	// status is the observed state.
	// +optional
	Status TunnelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TunnelList contains a list of Tunnel.
type TunnelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tunnel `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &Tunnel{}, &TunnelList{})
		return nil
	})
}
