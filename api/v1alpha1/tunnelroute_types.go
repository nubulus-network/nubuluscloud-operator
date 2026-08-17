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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Condition types set on a TunnelRoute.
const (
	// TunnelRouteConditionResolved is whether the upstream this route points at
	// could be turned into an address: the Service exists and publishes the
	// port. It is checked before anything is written to the platform, so a typo
	// in a Service name never becomes a route that answers 502.
	TunnelRouteConditionResolved = "Resolved"

	// TunnelRouteConditionSynced is whether the route exists in the platform
	// with these settings.
	TunnelRouteConditionSynced = "Synced"

	// TunnelRouteConditionReady is the roll-up.
	TunnelRouteConditionReady = "Ready"
)

// The values spec.type and spec.scheme may take. They are the CRD's enums, and
// having them as constants is what keeps the schema, the controller and the
// tests from drifting apart over a typo in a string literal.
const (
	// RouteTypeHost takes everything for a hostname that no path route claimed.
	RouteTypeHost = "host"
	// RouteTypePath takes one path prefix of a hostname.
	RouteTypePath = "path"

	// SchemeHTTP and SchemeHTTPS are how the agent reaches the upstream. The
	// public side is always HTTPS whichever of these is chosen.
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"
)

// TunnelRouteFinalizer keeps the object until the route has been deleted in the
// platform. A route that outlives its object still answers on a public
// hostname, which is the failure worth preventing.
const TunnelRouteFinalizer = "tunnel.nubulusnetwork.es/finalizer"

// ServiceTarget is a Service in this namespace.
//
// It may only be a Service in THIS namespace, and that restriction is the
// point rather than a simplification. A route publishes something on the
// internet; letting an object in one namespace name a Service in another would
// mean anyone who can create a TunnelRoute can expose any workload in the
// cluster, which is a wider grant than the namespace they were given.
type ServiceTarget struct {
	// name of the Service.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// port of the Service: either its number or the name it was declared with.
	// A name is resolved against the Service, so it keeps working when the
	// number behind it changes.
	// +kubebuilder:validation:XIntOrString
	Port intstr.IntOrString `json:"port"`
}

// UpstreamTarget is an address the agent can reach that is not a Service in
// this namespace: something outside the cluster, or a Service addressed by its
// fully qualified name on purpose.
//
// Nothing validates it. That is the trade for the escape hatch, and it is why
// the Service form is the one to reach for first.
type UpstreamTarget struct {
	// host is a hostname or an address, resolved by the agent pod.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// port on that host.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// TunnelRouteSpec is the desired state of a TunnelRoute.
//
// +kubebuilder:validation:XValidation:rule="has(self.service) != has(self.upstream)",message="set exactly one of service or upstream"
// +kubebuilder:validation:XValidation:rule="self.type != 'path' || (has(self.pathPrefix) && self.pathPrefix != '/')",message="a path route needs a pathPrefix other than '/'"
// +kubebuilder:validation:XValidation:rule="self.type != 'host' || !has(self.pathPrefix) || self.pathPrefix == '/'",message="a host route cannot have a pathPrefix; use type: path"
type TunnelRouteSpec struct {
	// tunnelRef is the Tunnel this route goes through, by name, in this
	// namespace. Same-namespace for the same reason as the Service: the tunnel
	// belongs to an account, and reaching one from another namespace would be
	// borrowing that account.
	// +kubebuilder:validation:MinLength=1
	TunnelRef string `json:"tunnelRef"`

	// hostname is the public name traffic arrives on. It must be pointed at the
	// tunnel's cnameTarget in DNS, which is a step outside this cluster.
	//
	// A hostname may only be routed by one account platform-wide, so a
	// conflict here is with somebody else and cannot be resolved from here.
	// +kubebuilder:validation:Pattern=`^([a-zA-Z0-9_]([-a-zA-Z0-9_]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`
	Hostname string `json:"hostname"`

	// type is how the route matches. A host route takes everything for the
	// hostname that no path route claimed; a path route takes one prefix.
	// +kubebuilder:validation:Enum=host;path
	// +kubebuilder:default=host
	// +optional
	Type string `json:"type,omitempty"`

	// pathPrefix is the prefix a path route matches. Required for type: path,
	// and it cannot be "/", which is what a host route is.
	// +kubebuilder:validation:Pattern=`^/.*$`
	// +optional
	PathPrefix string `json:"pathPrefix,omitempty"`

	// service is the workload in this namespace traffic is sent to.
	// +optional
	Service *ServiceTarget `json:"service,omitempty"`

	// upstream is an address to send traffic to instead of a Service.
	// +optional
	Upstream *UpstreamTarget `json:"upstream,omitempty"`

	// scheme the agent speaks to the upstream. The public side is always HTTPS
	// with a certificate the platform manages; this is only the last hop,
	// inside the cluster.
	// +kubebuilder:validation:Enum=http;https
	// +kubebuilder:default=http
	// +optional
	Scheme string `json:"scheme,omitempty"`

	// stripPrefix removes pathPrefix before the request reaches the upstream,
	// for a service that does not know it is mounted under one.
	// +kubebuilder:default=false
	// +optional
	StripPrefix bool `json:"stripPrefix,omitempty"`

	// priority orders the routes of a tunnel when more than one could match.
	// Lower wins.
	//
	// A POINTER, and it has to stay one. With a plain int32 the `omitempty` in
	// the tag drops a priority of 0 from the serialised object, the API server
	// sees an absent field, and the default of 100 is applied: the one value a
	// user might reasonably want most is the one they could not express.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=100
	// +optional
	Priority *int32 `json:"priority,omitempty"`

	// enabled is whether the route carries traffic. A disabled route keeps its
	// hostname claimed, which is the reason to disable one rather than delete
	// it.
	//
	// A POINTER for the same reason as priority, and here it is worse: false is
	// a bool's zero value, so `enabled: false` would vanish on the way in and
	// come back defaulted to true. The route would carry traffic while the
	// object said it did not.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// TunnelRouteStatus is the observed state of a TunnelRoute.
type TunnelRouteStatus struct {
	// routeID is the identifier the platform gave this route.
	// +optional
	RouteID string `json:"routeID,omitempty"`

	// tunnelID is the tunnel it was created in, resolved from tunnelRef.
	// +optional
	TunnelID string `json:"tunnelID,omitempty"`

	// upstreamHost is the address this route was published with: the in-cluster
	// name a Service resolved to, or the host given verbatim. It is here
	// because it is the one thing the spec does not say outright and the first
	// thing to check when a route answers the wrong thing.
	// +optional
	UpstreamHost string `json:"upstreamHost,omitempty"`

	// upstreamPort likewise, after a named port was resolved.
	// +optional
	UpstreamPort int32 `json:"upstreamPort,omitempty"`

	// observedGeneration is the generation of the spec this status was
	// computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions of the route.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=tunroute
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.spec.hostname`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.spec.pathPrefix`
// +kubebuilder:printcolumn:name="Upstream",type=string,JSONPath=`.status.upstreamHost`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TunnelRoute publishes one workload of this cluster on a public hostname,
// through a Tunnel.
type TunnelRoute struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state.
	// +required
	Spec TunnelRouteSpec `json:"spec"`

	// status is the observed state.
	// +optional
	Status TunnelRouteStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TunnelRouteList contains a list of TunnelRoute.
type TunnelRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TunnelRoute `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &TunnelRoute{}, &TunnelRouteList{})
		return nil
	})
}
