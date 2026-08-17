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

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	tunnelv1alpha1 "github.com/nubulus-network/nubuluscloud-operator/api/v1alpha1"
	"github.com/nubulus-network/nubuluscloud-operator/internal/nubulus"
)

func hostRoute() *tunnelv1alpha1.TunnelRoute {
	return &tunnelv1alpha1.TunnelRoute{
		Spec: tunnelv1alpha1.TunnelRouteSpec{
			TunnelRef: tunnelName,
			Hostname:  hostname,
			Type:      tunnelv1alpha1.RouteTypeHost,
			Scheme:    tunnelv1alpha1.SchemeHTTP,
			Priority:  ptr.To[int32](100),
			Enabled:   ptr.To(true),
		},
	}
}

// A host route leaves pathPrefix empty and the platform stores "/". Comparing
// the spec against the stored value directly would make every host route look
// like drift on every single reconcile, which is a rewrite of the whole route
// set every five minutes for as long as it exists.
func TestAHostRouteWithNoPathPrefixDoesNotLookLikeDrift(t *testing.T) {
	route := hostRoute()

	actual := &nubulus.Route{
		Type:           "host",
		Hostname:       hostname,
		PathPrefix:     "/",
		UpstreamHost:   upstreamName,
		UpstreamPort:   8080,
		UpstreamScheme: tunnelv1alpha1.SchemeHTTP,
		Priority:       100,
		Enabled:        true,
	}

	if identityChanged(actual, route) {
		t.Error("identityChanged reported a change on a route that matches")
	}
	if _, changed := routeDrift(actual, route, upstreamName, 8080); changed {
		t.Error("routeDrift reported a change on a route that matches")
	}
}

// The create has no `enabled` field, so a route is always born enabled. A spec
// asking for a disabled one is therefore only reachable through the update that
// follows, and the drift check is what triggers it.
func TestARouteAskedToBeDisabledDriftsUntilItIs(t *testing.T) {
	route := hostRoute()
	route.Spec.Enabled = ptr.To(false)

	justCreated := &nubulus.Route{
		Type: tunnelv1alpha1.RouteTypeHost, Hostname: hostname, PathPrefix: "/",
		UpstreamHost: upstreamName, UpstreamPort: 8080,
		UpstreamScheme: tunnelv1alpha1.SchemeHTTP, Priority: 100,
		Enabled: true, // what a create always returns
	}

	update, changed := routeDrift(justCreated, route, upstreamName, 8080)
	if !changed {
		t.Fatal("a route created enabled but specified disabled must drift")
	}
	if update.Enabled == nil || *update.Enabled {
		t.Errorf("the update must set enabled to false, got %v", update.Enabled)
	}
}

// The create reads a priority of 0 as "unset" and stores 100, while the update
// can genuinely set 0 because it arrives as a pointer. Without the drift check
// after a create, a spec asking for 0 would silently be 100 forever.
func TestAPriorityOfZeroSurvivesTheCreateDefaulting(t *testing.T) {
	route := hostRoute()
	route.Spec.Priority = ptr.To[int32](0)

	justCreated := &nubulus.Route{
		Type: tunnelv1alpha1.RouteTypeHost, Hostname: hostname, PathPrefix: "/",
		UpstreamHost: upstreamName, UpstreamPort: 8080,
		UpstreamScheme: tunnelv1alpha1.SchemeHTTP,
		Priority:       100, // the create turned 0 into the default
		Enabled:        true,
	}

	update, changed := routeDrift(justCreated, route, upstreamName, 8080)
	if !changed {
		t.Fatal("a priority of 0 defaulted to 100 by the create must drift")
	}
	if update.Priority == nil || *update.Priority != 0 {
		t.Errorf("the update must set priority to 0, got %v", update.Priority)
	}
}

// Type, hostname and path prefix are not in the update body at all, so a change
// to any of them has to be a delete and a create. Reporting it as ordinary
// drift would produce an update that silently changes nothing.
func TestTheFieldsTheAPICannotUpdateAreReportedAsAnIdentityChange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*tunnelv1alpha1.TunnelRoute)
	}{
		{"hostname", func(r *tunnelv1alpha1.TunnelRoute) { r.Spec.Hostname = otherHost }},
		{"type and prefix", func(r *tunnelv1alpha1.TunnelRoute) {
			r.Spec.Type = tunnelv1alpha1.RouteTypePath
			r.Spec.PathPrefix = "/api"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := hostRoute()
			tc.apply(route)

			actual := &nubulus.Route{
				Type: tunnelv1alpha1.RouteTypeHost, Hostname: hostname, PathPrefix: "/",
			}
			if !identityChanged(actual, route) {
				t.Error("a change to a field the API cannot update must force a replacement")
			}
		})
	}
}

func TestServicePortResolvesANameAgainstTheService(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "metrics", Port: 9090},
			{Name: "http", Port: 8080},
		}},
	}

	got, err := servicePort(svc, intstr.FromString("http"))
	if err != nil {
		t.Fatalf("resolving a declared port name: %v", err)
	}
	if got != 8080 {
		t.Errorf("port = %d, want 8080", got)
	}
}

// A port the Service does not publish cannot work. Refusing it here costs a
// condition on the object; letting it through costs a public hostname that
// answers nothing, and looks like a platform fault.
func TestServicePortRefusesAPortTheServiceDoesNotPublish(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}},
	}

	if _, err := servicePort(svc, intstr.FromInt32(9999)); err == nil {
		t.Error("a port the Service does not publish must be refused")
	}
	if _, err := servicePort(svc, intstr.FromString("grpc")); err == nil {
		t.Error("a port name the Service does not declare must be refused")
	}
}
