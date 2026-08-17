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
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tunnelv1alpha1 "github.com/Nubulus-Network/nubuluscloud-operator/api/v1alpha1"
)

func newService(t *testing.T, namespace, name string, port int32, portName string) {
	t.Helper()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name: portName, Port: port, TargetPort: intstr.FromInt32(port),
			}},
		},
	}
	if err := k8sClient.Create(context.Background(), svc); err != nil {
		t.Fatalf("creating the Service: %v", err)
	}
}

func newRouteReconciler(api APIClientFactory) *TunnelRouteReconciler {
	return &TunnelRouteReconciler{
		Client:        k8sClient,
		Scheme:        scheme.Scheme,
		Recorder:      record.NewFakeRecorder(100),
		API:           api,
		ClusterDomain: "cluster.local",
	}
}

func reconcileRoute(t *testing.T, r *TunnelRouteReconciler, obj client.Object) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	}); err != nil {
		t.Fatalf("reconciling the route: %v", err)
	}
}

func getRoute(t *testing.T, ns, name string) *tunnelv1alpha1.TunnelRoute {
	t.Helper()
	var out tunnelv1alpha1.TunnelRoute
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: name}, &out); err != nil {
		t.Fatalf("reading the TunnelRoute back: %v", err)
	}
	return &out
}

// readyTunnel sets up a namespace with a credential and a tunnel already
// created on the platform, which is the starting point of every route test.
func readyTunnel(t *testing.T, api *fakeAPI) (namespace string, tunnelID string) {
	t.Helper()
	ns := newNamespace(t)
	cred := newCredentialSecret(t, ns)
	tunnel := newTunnel(t, ns, "produccion", cred)
	reconcileTunnel(t, newTunnelReconciler(api.clientFactory()), tunnel)
	return ns, getTunnel(t, ns, "produccion").Status.TunnelID
}

func newRoute(t *testing.T, ns, name string, spec tunnelv1alpha1.TunnelRouteSpec) *tunnelv1alpha1.TunnelRoute {
	t.Helper()
	route := &tunnelv1alpha1.TunnelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
	if err := k8sClient.Create(context.Background(), route); err != nil {
		t.Fatalf("creating the TunnelRoute: %v", err)
	}
	return route
}

func hostRouteSpec(service string, port intstr.IntOrString) tunnelv1alpha1.TunnelRouteSpec {
	return tunnelv1alpha1.TunnelRouteSpec{
		TunnelRef: "produccion",
		Hostname:  "app.example.com",
		Type:      "host",
		Service:   &tunnelv1alpha1.ServiceTarget{Name: service, Port: port},
		Scheme:    "http",
		Priority:  ptr.To[int32](100),
		Enabled:   ptr.To(true),
	}
}

// The Service is turned into the name the agent will resolve, and it is a fully
// qualified one because the agent may be in another namespace: the short form
// would resolve relative to the agent's own.
func TestARouteIsPublishedWithTheClusterNameOfItsService(t *testing.T) {
	api := newFakeAPI(t)
	ns, tunnelID := readyTunnel(t, api)
	newService(t, ns, "web", 8080, "http")

	route := newRoute(t, ns, "web", hostRouteSpec("web", intstr.FromInt32(8080)))
	reconcileRoute(t, newRouteReconciler(api.clientFactory()), route)

	got := getRoute(t, ns, "web")
	want := "web." + ns + ".svc.cluster.local"
	if got.Status.UpstreamHost != want {
		t.Errorf("upstreamHost = %q, want %q", got.Status.UpstreamHost, want)
	}
	if got.Status.RouteID == "" {
		t.Error("the status carries no route id")
	}

	published := api.routesOf(tunnelID)
	if len(published) != 1 {
		t.Fatalf("the platform holds %d routes, want 1", len(published))
	}
	if published[0].UpstreamHost != want {
		t.Errorf("the published upstream is %q, want %q", published[0].UpstreamHost, want)
	}
}

func TestANamedPortIsResolvedAgainstTheService(t *testing.T) {
	api := newFakeAPI(t)
	ns, tunnelID := readyTunnel(t, api)
	newService(t, ns, "web", 8080, "http")

	route := newRoute(t, ns, "web", hostRouteSpec("web", intstr.FromString("http")))
	reconcileRoute(t, newRouteReconciler(api.clientFactory()), route)

	published := api.routesOf(tunnelID)
	if len(published) != 1 || published[0].UpstreamPort != 8080 {
		t.Fatalf("the named port did not resolve to 8080, got %+v", published)
	}
}

// A Service of the same name in another namespace must not satisfy a route.
// If it did, anyone able to create a TunnelRoute in their own namespace could
// publish another team's workload on the internet.
func TestAServiceInAnotherNamespaceDoesNotSatisfyARoute(t *testing.T) {
	api := newFakeAPI(t)
	ns, tunnelID := readyTunnel(t, api)

	elsewhere := newNamespace(t)
	newService(t, elsewhere, "web", 8080, "http")

	route := newRoute(t, ns, "web", hostRouteSpec("web", intstr.FromInt32(8080)))
	reconcileRoute(t, newRouteReconciler(api.clientFactory()), route)

	if n := api.routeCount(tunnelID); n != 0 {
		t.Errorf("the platform holds %d routes: a Service from another namespace was published", n)
	}

	got := getRoute(t, ns, "web")
	if meta.IsStatusConditionTrue(got.Status.Conditions, tunnelv1alpha1.TunnelRouteConditionResolved) {
		t.Error("Resolved is True even though the Service is in a different namespace")
	}
}

// Same reasoning for the tunnel itself: it carries the account, so reaching one
// from another namespace would be borrowing that account.
func TestATunnelInAnotherNamespaceDoesNotSatisfyARoute(t *testing.T) {
	api := newFakeAPI(t)
	_, tunnelID := readyTunnel(t, api)

	elsewhere := newNamespace(t)
	newService(t, elsewhere, "web", 8080, "http")
	route := newRoute(t, elsewhere, "web", hostRouteSpec("web", intstr.FromInt32(8080)))
	reconcileRoute(t, newRouteReconciler(api.clientFactory()), route)

	if n := api.routeCount(tunnelID); n != 0 {
		t.Errorf("the platform holds %d routes: a tunnel from another namespace was used", n)
	}
	got := getRoute(t, elsewhere, "web")
	synced := meta.FindStatusCondition(got.Status.Conditions, tunnelv1alpha1.TunnelRouteConditionSynced)
	if synced == nil || synced.Reason != "TunnelNotFound" {
		t.Errorf("Synced reason = %v, want TunnelNotFound", synced)
	}
}

// Type, hostname and path prefix are not in the update body, so changing one
// has to become a delete and a create. An update would silently change nothing.
func TestChangingTheHostnameReplacesTheRouteInsteadOfUpdatingIt(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI(t)
	ns, tunnelID := readyTunnel(t, api)
	newService(t, ns, "web", 8080, "http")

	route := newRoute(t, ns, "web", hostRouteSpec("web", intstr.FromInt32(8080)))
	r := newRouteReconciler(api.clientFactory())
	reconcileRoute(t, r, route)

	firstID := getRoute(t, ns, "web").Status.RouteID

	updated := getRoute(t, ns, "web")
	updated.Spec.Hostname = "otro.example.com"
	if err := k8sClient.Update(ctx, updated); err != nil {
		t.Fatalf("changing the hostname: %v", err)
	}
	reconcileRoute(t, r, route)

	published := api.routesOf(tunnelID)
	if len(published) != 1 {
		t.Fatalf("the platform holds %d routes, want 1: the old one was not removed", len(published))
	}
	if published[0].Hostname != "otro.example.com" {
		t.Errorf("hostname = %q, want the new one", published[0].Hostname)
	}
	if published[0].ID == firstID {
		t.Error("the route kept its id, so it was updated rather than replaced")
	}
}

// A route is always created enabled, because the create body has no field for
// it. Landing on a disabled one needs the update that follows the create.
func TestARouteSpecifiedDisabledEndsUpDisabled(t *testing.T) {
	api := newFakeAPI(t)
	ns, tunnelID := readyTunnel(t, api)
	newService(t, ns, "web", 8080, "http")

	spec := hostRouteSpec("web", intstr.FromInt32(8080))
	spec.Enabled = ptr.To(false)
	route := newRoute(t, ns, "web", spec)
	reconcileRoute(t, newRouteReconciler(api.clientFactory()), route)

	published := api.routesOf(tunnelID)
	if len(published) != 1 {
		t.Fatalf("the platform holds %d routes, want 1", len(published))
	}
	if published[0].Enabled {
		t.Error("the route is enabled: the update after the create did not happen")
	}
}

// The same shape as the tunnel: the reconcile has to be a no-op the second time
// round, or every resync rewrites every route.
func TestASecondReconcileChangesNothing(t *testing.T) {
	api := newFakeAPI(t)
	ns, tunnelID := readyTunnel(t, api)
	newService(t, ns, "web", 8080, "http")

	route := newRoute(t, ns, "web", hostRouteSpec("web", intstr.FromInt32(8080)))
	r := newRouteReconciler(api.clientFactory())
	reconcileRoute(t, r, route)
	first := api.routesOf(tunnelID)

	reconcileRoute(t, r, route)
	second := api.routesOf(tunnelID)

	if len(second) != 1 {
		t.Fatalf("the platform holds %d routes after a second reconcile, want 1", len(second))
	}
	if first[0].ID != second[0].ID {
		t.Error("the route was replaced on an unchanged reconcile")
	}
}

func TestDeletingTheObjectRemovesTheRoute(t *testing.T) {
	ctx := context.Background()
	api := newFakeAPI(t)
	ns, tunnelID := readyTunnel(t, api)
	newService(t, ns, "web", 8080, "http")

	route := newRoute(t, ns, "web", hostRouteSpec("web", intstr.FromInt32(8080)))
	r := newRouteReconciler(api.clientFactory())
	reconcileRoute(t, r, route)

	if err := k8sClient.Delete(ctx, getRoute(t, ns, "web")); err != nil {
		t.Fatalf("deleting the TunnelRoute: %v", err)
	}
	reconcileRoute(t, r, route)

	if n := api.routeCount(tunnelID); n != 0 {
		t.Errorf("the platform holds %d routes: one outlived its object on a public hostname", n)
	}
}

// The CRD's own validation, which is what makes a mistake fail at apply time
// instead of becoming a condition nobody reads.
func TestTheCRDRefusesSpecsThatCannotWork(t *testing.T) {
	ns := newNamespace(t)

	for _, tc := range []struct {
		name string
		spec tunnelv1alpha1.TunnelRouteSpec
		want string
	}{
		{
			name: "neither service nor upstream",
			spec: tunnelv1alpha1.TunnelRouteSpec{
				TunnelRef: "produccion", Hostname: "app.example.com", Type: "host",
			},
			want: "exactly one",
		},
		{
			name: "both service and upstream",
			spec: tunnelv1alpha1.TunnelRouteSpec{
				TunnelRef: "produccion", Hostname: "app.example.com", Type: "host",
				Service:  &tunnelv1alpha1.ServiceTarget{Name: "web", Port: intstr.FromInt32(80)},
				Upstream: &tunnelv1alpha1.UpstreamTarget{Host: "elsewhere", Port: 80},
			},
			want: "exactly one",
		},
		{
			name: "a path route with no prefix",
			spec: tunnelv1alpha1.TunnelRouteSpec{
				TunnelRef: "produccion", Hostname: "app.example.com", Type: "path",
				Service: &tunnelv1alpha1.ServiceTarget{Name: "web", Port: intstr.FromInt32(80)},
			},
			want: "pathPrefix",
		},
		{
			name: "a hostname that is not an FQDN",
			spec: tunnelv1alpha1.TunnelRouteSpec{
				TunnelRef: "produccion", Hostname: "app", Type: "host",
				Service: &tunnelv1alpha1.ServiceTarget{Name: "web", Port: intstr.FromInt32(80)},
			},
			want: "hostname",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := &tunnelv1alpha1.TunnelRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-" + strings.ReplaceAll(tc.name, " ", "-"),
					Namespace: ns,
				},
				Spec: tc.spec,
			}
			err := k8sClient.Create(context.Background(), route)
			if err == nil {
				t.Fatal("the API server accepted a spec that cannot work")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not mention %q, so it will not help anybody: %v", tc.want, err)
			}
		})
	}
}
