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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tunnelv1alpha1 "github.com/Nubulus-Network/nubuluscloud-operator/api/v1alpha1"
	"github.com/Nubulus-Network/nubuluscloud-operator/internal/nubulus"
)

// serviceNameIndex indexes routes by the Service they point at, so that a
// Service appearing or changing wakes the routes that need it.
//
// Without it a TunnelRoute applied in the same breath as its Service (which is
// what everybody does, they are in the same file) would fail to resolve and
// then sit unresolved until the next slow resync. The index turns that into a
// second.
const serviceNameIndex = ".spec.service.name"

// tunnelNotReadyRetry is how long to wait for a Tunnel that exists but has not
// finished being created yet. Short, because it is a state that clears on its
// own within one reconcile of the other controller.
const tunnelNotReadyRetry = 15 * time.Second

// TunnelRouteReconciler reconciles a TunnelRoute object.
type TunnelRouteReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	API APIClientFactory

	// ClusterDomain is the DNS suffix of this cluster, used to turn a Service
	// into the name the agent will resolve. It is a setting rather than a
	// constant because a cluster installed with a different one would produce
	// routes pointing at names that do not exist, and the failure would show up
	// as a 502 on the customer's site rather than as anything here.
	ClusterDomain string
}

// +kubebuilder:rbac:groups=tunnel.nubulusnetwork.es,resources=tunnelroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tunnel.nubulusnetwork.es,resources=tunnelroutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tunnel.nubulusnetwork.es,resources=tunnelroutes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch

// Reconcile brings one TunnelRoute and the platform into agreement.
func (r *TunnelRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var route tunnelv1alpha1.TunnelRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The tunnel is looked up first because everything else needs it: the
	// credential to talk to the platform is the TUNNEL's, not the route's. A
	// route carries no credential of its own, which is what keeps a namespace's
	// routes inside the account that namespace was given.
	var tunnel tunnelv1alpha1.Tunnel
	tunnelKey := types.NamespacedName{Namespace: route.Namespace, Name: route.Spec.TunnelRef}
	tunnelErr := r.Get(ctx, tunnelKey, &tunnel)

	if !route.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &route, &tunnel, tunnelErr)
	}

	if tunnelErr != nil {
		if !apierrors.IsNotFound(tunnelErr) {
			return ctrl.Result{}, tunnelErr
		}
		r.setSynced(&route, metav1.ConditionFalse, "TunnelNotFound",
			fmt.Sprintf("No Tunnel named %q in this namespace.", route.Spec.TunnelRef))
		r.setReady(&route)
		return r.finish(ctx, &route, ctrl.Result{RequeueAfter: permanentRetryPeriod}, nil)
	}

	// Same ordering rule as the tunnel: the finalizer goes on before anything
	// exists remotely. A route that outlives its object still answers on a
	// public hostname and still holds that hostname against the rest of the
	// platform.
	if controllerutil.AddFinalizer(&route, tunnelv1alpha1.TunnelRouteFinalizer) {
		if err := r.Update(ctx, &route); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding the finalizer: %w", err)
		}
	}

	if tunnel.Status.TunnelID == "" {
		r.setSynced(&route, metav1.ConditionFalse, "TunnelNotReady",
			"The Tunnel exists but has not been created on the platform yet.")
		r.setReady(&route)
		return r.finish(ctx, &route, ctrl.Result{RequeueAfter: tunnelNotReadyRetry}, nil)
	}

	// The upstream is resolved BEFORE anything is written to the platform. A
	// route published with a Service name that does not exist is a public
	// hostname answering 502, and it would look like a platform fault rather
	// than like the typo it is.
	host, port, err := r.resolveUpstream(ctx, &route)
	if err != nil {
		r.setResolved(&route, metav1.ConditionFalse, "UpstreamUnresolved", err.Error())
		r.setSynced(&route, metav1.ConditionFalse, "UpstreamUnresolved",
			"Not published: the upstream could not be resolved.")
		r.setReady(&route)
		r.Recorder.Eventf(&route, corev1.EventTypeWarning, "UpstreamUnresolved", "%v", err)
		return r.finish(ctx, &route, ctrl.Result{RequeueAfter: permanentRetryPeriod}, nil)
	}
	r.setResolved(&route, metav1.ConditionTrue, "Resolved",
		fmt.Sprintf("Sending traffic to %s:%d.", host, port))

	api, err := r.API.ClientFor(ctx, r.Client, &tunnel)
	if err != nil {
		r.setSynced(&route, metav1.ConditionFalse, "CredentialUnreadable", err.Error())
		r.setReady(&route)
		return r.finish(ctx, &route, ctrl.Result{RequeueAfter: permanentRetryPeriod}, nil)
	}

	remote, err := r.ensureRoute(ctx, api, &route, tunnel.Status.TunnelID, host, port)
	if err != nil {
		failure := nubulus.Classify(err)
		r.setSynced(&route, metav1.ConditionFalse, failure.Reason, failure.Message)
		r.setReady(&route)
		r.Recorder.Eventf(&route, corev1.EventTypeWarning, failure.Reason, "%s", failure.Message)
		if failure.Permanent {
			return r.finish(ctx, &route, ctrl.Result{RequeueAfter: permanentRetryPeriod}, nil)
		}
		return r.finish(ctx, &route, ctrl.Result{}, err)
	}

	route.Status.RouteID = remote.ID
	route.Status.TunnelID = tunnel.Status.TunnelID
	route.Status.UpstreamHost = host
	route.Status.UpstreamPort = port
	r.setSynced(&route, metav1.ConditionTrue, "Synced", "The route is published.")
	r.setReady(&route)

	return r.finish(ctx, &route, ctrl.Result{RequeueAfter: resyncPeriod}, nil)
}

// resolveUpstream turns the spec into the address the agent will connect to.
func (r *TunnelRouteReconciler) resolveUpstream(
	ctx context.Context,
	route *tunnelv1alpha1.TunnelRoute,
) (string, int32, error) {
	if u := route.Spec.Upstream; u != nil {
		return u.Host, u.Port, nil
	}

	target := route.Spec.Service
	var svc corev1.Service
	// The Service is read from the ROUTE's namespace, never from one named in
	// the spec, because there is no field to name one. See ServiceTarget.
	nn := types.NamespacedName{Namespace: route.Namespace, Name: target.Name}
	if err := r.Get(ctx, nn, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", 0, fmt.Errorf("no Service named %q in this namespace", target.Name)
		}
		return "", 0, err
	}

	port, err := servicePort(&svc, target.Port)
	if err != nil {
		return "", 0, err
	}

	// The fully qualified name rather than the short one: the agent may be in
	// another namespace, and the short form would resolve relative to its own.
	host := fmt.Sprintf("%s.%s.svc.%s", svc.Name, svc.Namespace, r.ClusterDomain)
	return host, port, nil
}

// servicePort resolves a port given as a number or as a declared name.
func servicePort(svc *corev1.Service, want intstr.IntOrString) (int32, error) {
	if want.Type == intstr.String {
		for _, p := range svc.Spec.Ports {
			if p.Name == want.StrVal {
				return p.Port, nil
			}
		}
		return 0, fmt.Errorf("Service %q declares no port named %q", svc.Name, want.StrVal)
	}

	n := int32(want.IntValue())
	for _, p := range svc.Spec.Ports {
		if p.Port == n {
			return n, nil
		}
	}
	// Refused rather than passed through. A port the Service does not publish
	// cannot work, and finding that out here costs a condition; finding it out
	// later costs a public hostname answering nothing.
	return 0, fmt.Errorf("Service %q does not publish port %d", svc.Name, n)
}

// ensureRoute makes the platform hold exactly the route this object describes.
func (r *TunnelRouteReconciler) ensureRoute(
	ctx context.Context,
	api *nubulus.Client,
	route *tunnelv1alpha1.TunnelRoute,
	tunnelID, host string,
	port int32,
) (*nubulus.Route, error) {
	log := logf.FromContext(ctx)

	existing, err := r.findRoute(ctx, api, route, tunnelID)
	if err != nil {
		return nil, err
	}

	// Type, hostname and path prefix cannot be updated: they are not in the
	// update body at all. In Terraform that is a forced replacement the user
	// has to approve; here the reconciler can simply do it, which is the one
	// place this operator has an easier job than the provider.
	//
	// Delete first, then create. Two routes with the same hostname and prefix
	// in one tunnel would both end up in the generated configuration, and which
	// one wins is not something worth finding out.
	if existing != nil && identityChanged(existing, route) {
		log.Info("the route's identity changed; replacing it",
			"routeID", existing.ID, "hostname", route.Spec.Hostname)
		if err := api.Tunnel.DeleteRoute(ctx, tunnelID, existing.ID); err != nil && !nubulus.IsNotFound(err) {
			return nil, fmt.Errorf("deleting the route being replaced: %w", err)
		}
		r.Recorder.Eventf(route, corev1.EventTypeNormal, "RouteReplaced",
			"Recreated the route because its hostname, type or path prefix changed")
		existing = nil
	}

	if existing == nil {
		created, err := api.Tunnel.CreateRoute(ctx, tunnelID, nubulus.CreateRouteInput{
			Type:           routeType(route),
			Hostname:       route.Spec.Hostname,
			PathPrefix:     pathPrefix(route),
			UpstreamHost:   host,
			UpstreamPort:   int(port),
			UpstreamScheme: upstreamScheme(route),
			StripPrefix:    route.Spec.StripPrefix,
			Priority:       int(priority(route)),
		})
		if err != nil {
			return nil, err
		}
		existing = created
	}

	// Reconciled after the create as well as after a find, and that is not
	// belt-and-braces. A create cannot express two of the states this spec can:
	// it has no `enabled` field, so every route is born enabled, and it reads a
	// priority of 0 as "unset" and turns it into the default. Comparing what
	// came back against what was asked for, always, is what makes both land
	// without a special case for either.
	update, changed := routeDrift(existing, route, host, port)
	if !changed {
		return existing, nil
	}
	return api.Tunnel.UpdateRoute(ctx, tunnelID, existing.ID, update)
}

// findRoute locates the route this object owns among the tunnel's routes.
//
// The identifier in the status is preferred, and the hostname and path prefix
// are the fallback. That fallback is what makes a create whose answer was lost
// recoverable: the pair is unique within a tunnel, so it identifies the route
// as well as its id does.
func (r *TunnelRouteReconciler) findRoute(
	ctx context.Context,
	api *nubulus.Client,
	route *tunnelv1alpha1.TunnelRoute,
	tunnelID string,
) (*nubulus.Route, error) {
	// One listing rather than a read per route: with a page of objects each
	// asking for its own, a resync would be one request per route per tick
	// against a single gateway.
	routes, err := api.Tunnel.ListRoutes(ctx, tunnelID)
	if err != nil {
		return nil, err
	}

	if id := route.Status.RouteID; id != "" {
		for i := range routes {
			if routes[i].ID == id {
				return &routes[i], nil
			}
		}
	}

	for i := range routes {
		if routes[i].Hostname == route.Spec.Hostname && routes[i].PathPrefix == pathPrefix(route) {
			return &routes[i], nil
		}
	}
	return nil, nil
}

// identityChanged reports whether the parts of a route that cannot be updated
// no longer match the spec.
func identityChanged(actual *nubulus.Route, route *tunnelv1alpha1.TunnelRoute) bool {
	return actual.Type != routeType(route) ||
		actual.Hostname != route.Spec.Hostname ||
		actual.PathPrefix != pathPrefix(route)
}

// routeDrift compares a route against the spec and returns the update that
// would close the gap, and whether there is one.
func routeDrift(
	actual *nubulus.Route,
	route *tunnelv1alpha1.TunnelRoute,
	host string,
	port int32,
) (nubulus.UpdateRouteInput, bool) {
	var in nubulus.UpdateRouteInput
	changed := false

	if actual.UpstreamHost != host {
		in.UpstreamHost = ptr.To(host)
		changed = true
	}
	if actual.UpstreamPort != int(port) {
		in.UpstreamPort = ptr.To(int(port))
		changed = true
	}
	if actual.UpstreamScheme != upstreamScheme(route) {
		in.UpstreamScheme = ptr.To(upstreamScheme(route))
		changed = true
	}
	if actual.StripPrefix != route.Spec.StripPrefix {
		in.StripPrefix = ptr.To(route.Spec.StripPrefix)
		changed = true
	}
	if actual.Priority != int(priority(route)) {
		in.Priority = ptr.To(int(priority(route)))
		changed = true
	}
	if actual.Enabled != enabled(route) {
		in.Enabled = ptr.To(enabled(route))
		changed = true
	}
	return in, changed
}

func routeType(route *tunnelv1alpha1.TunnelRoute) string {
	if route.Spec.Type == "" {
		return "host"
	}
	return route.Spec.Type
}

// pathPrefix is what the platform stores, which is "/" for a host route even
// though the spec leaves it empty. Comparing against the stored form is what
// keeps a host route from looking like drift on every reconcile.
func pathPrefix(route *tunnelv1alpha1.TunnelRoute) string {
	if route.Spec.PathPrefix == "" {
		return "/"
	}
	return route.Spec.PathPrefix
}

// priority and enabled read the two pointer fields. They are pointers so that
// a zero value survives the round trip through the API server; see the spec.
// The defaults here are the CRD's, repeated for the case where an object was
// built in Go and never went through defaulting, which is every unit test.
func priority(route *tunnelv1alpha1.TunnelRoute) int32 {
	if route.Spec.Priority == nil {
		return 100
	}
	return *route.Spec.Priority
}

func enabled(route *tunnelv1alpha1.TunnelRoute) bool {
	if route.Spec.Enabled == nil {
		return true
	}
	return *route.Spec.Enabled
}

func upstreamScheme(route *tunnelv1alpha1.TunnelRoute) string {
	if route.Spec.Scheme == "" {
		return "http"
	}
	return route.Spec.Scheme
}

// reconcileDelete removes the route from the platform and then the finalizer.
func (r *TunnelRouteReconciler) reconcileDelete(
	ctx context.Context,
	route *tunnelv1alpha1.TunnelRoute,
	tunnel *tunnelv1alpha1.Tunnel,
	tunnelErr error,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(route, tunnelv1alpha1.TunnelRouteFinalizer) {
		return ctrl.Result{}, nil
	}

	// A route lives inside its tunnel, so a tunnel that is gone took its routes
	// with it. Holding the object in Terminating waiting for a tunnel that will
	// never come back would be a finalizer that never clears, and deleting a
	// whole Tunnel with its routes is an ordinary thing to do.
	switch {
	case tunnelErr != nil && apierrors.IsNotFound(tunnelErr):
		log.Info("the tunnel is gone; the route went with it")
	case tunnelErr != nil:
		return ctrl.Result{}, tunnelErr
	case tunnel.Status.TunnelID == "":
		log.Info("the tunnel was never created; nothing to delete")
	default:
		if err := r.deleteRemoteRoute(ctx, route, tunnel.Status.TunnelID); err != nil {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(route, tunnelv1alpha1.TunnelRouteFinalizer)
	return ctrl.Result{}, r.Update(ctx, route)
}

func (r *TunnelRouteReconciler) deleteRemoteRoute(
	ctx context.Context,
	route *tunnelv1alpha1.TunnelRoute,
	tunnelID string,
) error {
	// The tunnel object is still there, so its credential should be readable;
	// if it is not, the same reasoning as for a tunnel applies: refusing to
	// finish is better than abandoning a live public hostname.
	var tunnel tunnelv1alpha1.Tunnel
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: route.Namespace, Name: route.Spec.TunnelRef,
	}, &tunnel); err != nil {
		return err
	}

	api, err := r.API.ClientFor(ctx, r.Client, &tunnel)
	if err != nil {
		r.Recorder.Eventf(route, corev1.EventTypeWarning, "DeleteBlocked",
			"Cannot delete the route because the tunnel's credential could not be read: %v", err)
		return err
	}

	id := route.Status.RouteID
	if id == "" {
		// Same lost-answer window as everywhere else: a route may exist that
		// the status never recorded. The hostname and prefix identify it.
		found, err := r.findRoute(ctx, api, route, tunnelID)
		if err != nil {
			return err
		}
		if found == nil {
			return nil
		}
		id = found.ID
	}

	if err := api.Tunnel.DeleteRoute(ctx, tunnelID, id); err != nil && !nubulus.IsNotFound(err) {
		return fmt.Errorf("deleting the route: %w", err)
	}
	return nil
}

func (r *TunnelRouteReconciler) finish(
	ctx context.Context,
	route *tunnelv1alpha1.TunnelRoute,
	result ctrl.Result,
	reconcileErr error,
) (ctrl.Result, error) {
	route.Status.ObservedGeneration = route.Generation
	if err := r.Status().Update(ctx, route); err != nil {
		if reconcileErr != nil {
			return result, reconcileErr
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return result, reconcileErr
}

func (r *TunnelRouteReconciler) setResolved(
	route *tunnelv1alpha1.TunnelRoute, status metav1.ConditionStatus, reason, msg string,
) {
	meta.SetStatusCondition(&route.Status.Conditions, metav1.Condition{
		Type:               tunnelv1alpha1.TunnelRouteConditionResolved,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: route.Generation,
	})
}

func (r *TunnelRouteReconciler) setSynced(
	route *tunnelv1alpha1.TunnelRoute, status metav1.ConditionStatus, reason, msg string,
) {
	meta.SetStatusCondition(&route.Status.Conditions, metav1.Condition{
		Type:               tunnelv1alpha1.TunnelRouteConditionSynced,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: route.Generation,
	})
}

// setReady rolls up the other two.
//
// It says the route is published and points somewhere real. It does NOT say
// traffic arrives: that also needs the hostname pointed at the tunnel in DNS,
// which is a step outside this cluster and outside this operator.
func (r *TunnelRouteReconciler) setReady(route *tunnelv1alpha1.TunnelRoute) {
	resolved := meta.IsStatusConditionTrue(route.Status.Conditions, tunnelv1alpha1.TunnelRouteConditionResolved)
	synced := meta.IsStatusConditionTrue(route.Status.Conditions, tunnelv1alpha1.TunnelRouteConditionSynced)

	status, reason, msg := metav1.ConditionFalse, "NotReady", "The route is not ready."
	switch {
	case !resolved:
		reason, msg = "UpstreamUnresolved", "The upstream could not be resolved."
	case !synced:
		reason, msg = "NotSynced", "The route is not published."
	default:
		status, reason = metav1.ConditionTrue, "Published"
		msg = "The route is published. Traffic also needs the hostname pointed at the tunnel in DNS."
	}

	meta.SetStatusCondition(&route.Status.Conditions, metav1.Condition{
		Type:               tunnelv1alpha1.TunnelRouteConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: route.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *TunnelRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &tunnelv1alpha1.TunnelRoute{}, serviceNameIndex,
		func(obj client.Object) []string {
			route, ok := obj.(*tunnelv1alpha1.TunnelRoute)
			if !ok || route.Spec.Service == nil {
				return nil
			}
			return []string{route.Spec.Service.Name}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&tunnelv1alpha1.TunnelRoute{}).
		// A Tunnel becoming ready is what unblocks every route waiting on it.
		Watches(
			&tunnelv1alpha1.Tunnel{},
			handler.EnqueueRequestsFromMapFunc(r.routesOfTunnel),
		).
		Watches(
			&corev1.Service{},
			handler.EnqueueRequestsFromMapFunc(r.routesOfService),
		).
		Named("tunnelroute").
		Complete(r)
}

func (r *TunnelRouteReconciler) routesOfTunnel(ctx context.Context, obj client.Object) []reconcile.Request {
	var routes tunnelv1alpha1.TunnelRouteList
	if err := r.List(ctx, &routes, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range routes.Items {
		if routes.Items[i].Spec.TunnelRef == obj.GetName() {
			out = append(out, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&routes.Items[i]),
			})
		}
	}
	return out
}

func (r *TunnelRouteReconciler) routesOfService(ctx context.Context, obj client.Object) []reconcile.Request {
	var routes tunnelv1alpha1.TunnelRouteList
	if err := r.List(ctx, &routes,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{serviceNameIndex: obj.GetName()},
	); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(routes.Items))
	for i := range routes.Items {
		out = append(out, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&routes.Items[i]),
		})
	}
	return out
}
