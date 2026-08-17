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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	tunnelv1alpha1 "github.com/Nubulus-Network/nubuluscloud-operator/api/v1alpha1"
	"github.com/Nubulus-Network/nubuluscloud-operator/internal/nubulus"
)

const (
	// resyncPeriod is how often a healthy object is reconciled with nothing
	// having changed in the cluster.
	//
	// It exists because the platform has no way to tell the operator that
	// something changed on its side: a tunnel deleted in the panel, or a
	// connection that dropped, is only noticed by asking. It is deliberately
	// not short. Each tick is a request per object against a single gateway,
	// and nothing here is urgent enough to pay for a tighter loop. The agent
	// polls its own configuration every thirty seconds regardless of this.
	resyncPeriod = 5 * time.Minute

	// permanentRetryPeriod is how long the operator waits before looking again
	// at an object whose last failure cannot be fixed by retrying.
	//
	// The requeue is slow rather than absent because "permanent" is a judgement
	// about the request, not about the world: a hostname held by another
	// account gets released, a credential gets replaced. What it must never
	// become is a tight loop: an operator retries forever where a person runs
	// a command once, and the endpoint on the other side is not built for that.
	permanentRetryPeriod = 5 * time.Minute
)

// TunnelReconciler reconciles a Tunnel object.
type TunnelReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// API builds platform clients from the credential a Tunnel names.
	API APIClientFactory

	// DefaultAgentImage is used by any Tunnel that does not name one.
	DefaultAgentImage string
}

// +kubebuilder:rbac:groups=tunnel.nubulusnetwork.es,resources=tunnels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tunnel.nubulusnetwork.es,resources=tunnels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tunnel.nubulusnetwork.es,resources=tunnels/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile brings one Tunnel object and the platform into agreement.
func (r *TunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var tunnel tunnelv1alpha1.Tunnel
	if err := r.Get(ctx, req.NamespacedName, &tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tunnel.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &tunnel)
	}

	// THE FINALIZER GOES ON BEFORE ANYTHING IS CREATED REMOTELY, and the order
	// is the point. A tunnel created by an object that is then deleted without
	// a finalizer is a tunnel nothing refers to, holding an address from the
	// account's pool, that no one will ever find: the object that knew its
	// identifier is gone.
	//
	// Adding it first costs a write on the first reconcile and makes the
	// dangerous window empty.
	if controllerutil.AddFinalizer(&tunnel, tunnelv1alpha1.TunnelFinalizer) {
		if err := r.Update(ctx, &tunnel); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding the finalizer: %w", err)
		}
	}

	api, err := r.API.ClientFor(ctx, r.Client, &tunnel)
	if err != nil {
		// A missing or malformed Secret is the user's to fix, and retrying it
		// every few seconds helps nobody. The watch on Secrets is what picks
		// the fix up promptly.
		r.setSynced(&tunnel, metav1.ConditionFalse, "CredentialUnreadable", err.Error())
		return r.finish(ctx, &tunnel, ctrl.Result{RequeueAfter: permanentRetryPeriod}, nil)
	}

	// ── the tunnel itself ────────────────────────────────────────────────────
	remote, createdToken, err := r.ensureTunnel(ctx, api, &tunnel)
	if err != nil {
		return r.fail(ctx, &tunnel, "resolving the tunnel", err)
	}

	// ── its credential ───────────────────────────────────────────────────────
	secret, err := r.ensureCredentialSecret(ctx, api, &tunnel, remote.ID, createdToken)
	if err != nil {
		return r.fail(ctx, &tunnel, "storing the tunnel credential", err)
	}

	// ── the agent that uses it ───────────────────────────────────────────────
	deployment, err := r.ensureAgentDeployment(ctx, &tunnel, secret.ResourceVersion)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling the agent Deployment: %w", err)
	}

	tunnel.Status.TunnelID = remote.ID
	tunnel.Status.Subdomain = remote.TunnelSubdomain
	tunnel.Status.WireGuardIP = remote.WireGuardIP
	tunnel.Status.CredentialSecret = secret.Name
	tunnel.Status.OnlineStatus = remote.OnlineStatus
	// The platform serves a tunnel on its own subdomain, and that subdomain is
	// what a customer hostname is pointed at. The create says so explicitly;
	// a read does not, so it is derived here rather than left blank on every
	// reconcile after the first.
	tunnel.Status.CNAMETarget = remote.TunnelSubdomain

	r.setSynced(&tunnel, metav1.ConditionTrue, "Synced", "The tunnel exists and its credential is stored.")
	r.setAgentAvailable(&tunnel, deployment)
	r.setReady(&tunnel, remote.OnlineStatus)

	log.V(1).Info("reconciled", "tunnelID", remote.ID, "online", remote.OnlineStatus)
	return r.finish(ctx, &tunnel, ctrl.Result{RequeueAfter: resyncPeriod}, nil)
}

// ensureTunnel returns the tunnel this object owns, creating it if there is
// none. The second return is the credential, populated ONLY when this call is
// what created the tunnel.
func (r *TunnelReconciler) ensureTunnel(
	ctx context.Context,
	api *nubulus.Client,
	tunnel *tunnelv1alpha1.Tunnel,
) (*nubulus.Tunnel, string, error) {
	log := logf.FromContext(ctx)

	if id := tunnel.Status.TunnelID; id != "" {
		got, err := api.Tunnel.GetTunnel(ctx, id)
		switch {
		case err == nil:
			return got.Tunnel, "", nil
		case nubulus.IsNotFound(err):
			// Deleted on the platform behind our back. Falling through to the
			// create is the reconciler doing its job: the object still asks for
			// a tunnel to exist. The credential changes with it, which is why
			// the Secret is rewritten and the agent rolled.
			log.Info("the tunnel this object refers to no longer exists; creating a new one",
				"tunnelID", id)
		default:
			return nil, "", err
		}
	}

	// The UID identifies this object for the life of this object, and a new one
	// is minted if it is deleted and recreated, which is exactly the semantics
	// wanted, because a recreated object should get its own tunnel.
	externalID := string(tunnel.UID)

	// RECORDED BEFORE THE CALL, NOT AFTER, and this is the whole point of it.
	//
	// It marks "a tunnel may exist under this id from now on", which is what
	// the delete path reads to decide whether an unreadable credential is a
	// wall or an irrelevance. Written after the answer, it would be missing in
	// exactly the window where a tunnel exists and nothing here knows its
	// identifier: the case it is meant to cover.
	//
	// Placing it any earlier would be wrong in the other direction. A Tunnel
	// naming a Secret that does not exist never reaches this line, and holding
	// such an object in Terminating forever over a typo is not a trade worth
	// making.
	if tunnel.Status.ExternalID == "" {
		tunnel.Status.ExternalID = externalID
		if err := r.Status().Update(ctx, tunnel); err != nil {
			return nil, "", fmt.Errorf("recording the external id: %w", err)
		}
	}

	created, err := api.Tunnel.CreateTunnel(ctx, nubulus.CreateTunnelInput{
		Name:       tunnel.Spec.DisplayName,
		ExternalID: externalID,
	})
	if err != nil {
		return nil, "", err
	}

	if created.Adopted {
		// The tunnel was already there under this external id: a previous
		// attempt created it and its answer never made it into the status.
		// This is the case the whole external id mechanism exists for, and the
		// alternative to it is an orphan nobody can reach.
		//
		// No credential comes back with an adoption, on purpose. Whether one is
		// needed is decided by ensureCredentialSecret, which knows whether the
		// old one survived.
		log.Info("adopted an existing tunnel", "tunnelID", created.TunnelID)
	}

	got, err := api.Tunnel.GetTunnel(ctx, created.TunnelID)
	if err != nil {
		return nil, "", err
	}
	return got.Tunnel, created.TunnelToken, nil
}

// ensureCredentialSecret makes sure the Secret holding the tunnel token exists
// and is usable, and returns it.
//
// createdToken is the credential a create just produced, or empty. The empty
// case is the interesting one: it means the tunnel already existed, and the
// only way to obtain a credential for a tunnel that already exists is to rotate
// it, which breaks whatever is currently running with the old one.
//
// So rotation happens only when there is genuinely nothing usable: an existing
// Secret with a token in it is kept, never refreshed. Rotating "to be safe"
// would take a working tunnel down on every restart of the operator.
func (r *TunnelReconciler) ensureCredentialSecret(
	ctx context.Context,
	api *nubulus.Client,
	tunnel *tunnelv1alpha1.Tunnel,
	tunnelID string,
	createdToken string,
) (*corev1.Secret, error) {
	log := logf.FromContext(ctx)
	name := tokenSecretName(tunnel.Name)

	token := createdToken
	if token == "" {
		var existing corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Namespace: tunnel.Namespace, Name: name}, &existing)
		switch {
		case err == nil && len(existing.Data[tokenSecretKey]) > 0:
			return &existing, nil
		case err != nil && !apierrors.IsNotFound(err):
			return nil, err
		}

		log.Info("no usable credential for this tunnel; rotating", "tunnelID", tunnelID)
		rotated, err := api.Tunnel.RotateToken(ctx, tunnelID)
		if err != nil {
			return nil, fmt.Errorf("rotating the credential: %w", err)
		}
		token = rotated.TunnelToken
		r.Recorder.Eventf(tunnel, corev1.EventTypeNormal, "CredentialRotated",
			"Issued a new tunnel credential because none was stored")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: tunnel.Namespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = agentLabels(tunnel)
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[tokenSecretKey] = []byte(token)
		// Owned by the Tunnel, so a deleted Tunnel takes its credential with
		// it rather than leaving a live token in the namespace.
		return controllerutil.SetControllerReference(tunnel, secret, r.Scheme)
	}); err != nil {
		return nil, err
	}
	return secret, nil
}

// ensureAgentDeployment reconciles the Deployment running the client end.
func (r *TunnelReconciler) ensureAgentDeployment(
	ctx context.Context,
	tunnel *tunnelv1alpha1.Tunnel,
	credentialVersion string,
) (*appsv1.Deployment, error) {
	desired := buildAgentDeployment(tunnel, r.DefaultAgentImage, credentialVersion)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Labels = desired.Labels
		// The selector is immutable once set, so it is written on create and
		// then left alone: assigning it unconditionally would make every update
		// of an existing Deployment fail with a validation error rather than
		// with anything that explains itself.
		if deployment.Spec.Selector == nil {
			deployment.Spec.Selector = desired.Spec.Selector
		}
		deployment.Spec.Replicas = desired.Spec.Replicas
		deployment.Spec.Strategy = desired.Spec.Strategy
		deployment.Spec.Template = desired.Spec.Template
		return controllerutil.SetControllerReference(tunnel, deployment, r.Scheme)
	}); err != nil {
		return nil, err
	}
	return deployment, nil
}

// reconcileDelete removes the tunnel from the platform and then the finalizer.
func (r *TunnelReconciler) reconcileDelete(
	ctx context.Context,
	tunnel *tunnelv1alpha1.Tunnel,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(tunnel, tunnelv1alpha1.TunnelFinalizer) {
		return ctrl.Result{}, nil
	}

	// Nothing was ever attempted against the platform, so there is nothing to
	// clean up and no reason to keep the object.
	//
	// This case is common and the one below is rare, which is why they are
	// separated: a Tunnel naming a Secret that does not exist never reached the
	// API at all, and holding it in Terminating forever over a typo would mean
	// every such object had to be unstuck by editing its finalizers by hand.
	if tunnel.Status.ExternalID == "" {
		controllerutil.RemoveFinalizer(tunnel, tunnelv1alpha1.TunnelFinalizer)
		return ctrl.Result{}, r.Update(ctx, tunnel)
	}

	api, err := r.API.ClientFor(ctx, r.Client, tunnel)
	if err != nil {
		// Past that point something may exist, so a credential that cannot be
		// read is a wall: dropping the finalizer here would abandon a tunnel on
		// the platform. Better to hold the object in Terminating, where it is
		// visible, than to lose the only pointer to what needs cleaning up.
		log.Error(err, "cannot delete the tunnel: its credential is unreadable")
		r.Recorder.Eventf(tunnel, corev1.EventTypeWarning, "DeleteBlocked",
			"Cannot delete the tunnel because its credential could not be read: %v", err)
		return ctrl.Result{RequeueAfter: permanentRetryPeriod}, nil
	}

	id := tunnel.Status.TunnelID
	if id == "" {
		// The status never recorded an identifier, but a tunnel may still have
		// been created: this is the same lost-answer window the external id
		// exists for, seen from the delete side. Without this lookup a create
		// followed quickly by a delete would leak a tunnel permanently, because
		// the object that could have identified it is about to be gone.
		found, err := api.Tunnel.FindTunnelByExternalID(ctx, string(tunnel.UID))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("looking for a tunnel to clean up: %w", err)
		}
		if found != nil {
			id = found.ID
			log.Info("found an unrecorded tunnel to delete", "tunnelID", id)
		}
	}

	if id != "" {
		if err := api.Tunnel.DeleteTunnel(ctx, id); err != nil && !nubulus.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting the tunnel: %w", err)
		}
	}

	controllerutil.RemoveFinalizer(tunnel, tunnelv1alpha1.TunnelFinalizer)
	return ctrl.Result{}, r.Update(ctx, tunnel)
}

// fail records a failed reconcile and decides whether to retry hard or slowly.
func (r *TunnelReconciler) fail(
	ctx context.Context,
	tunnel *tunnelv1alpha1.Tunnel,
	action string,
	err error,
) (ctrl.Result, error) {
	failure := nubulus.Classify(err)
	r.setSynced(tunnel, metav1.ConditionFalse, failure.Reason, failure.Message)
	r.setReady(tunnel, "")
	r.Recorder.Eventf(tunnel, corev1.EventTypeWarning, failure.Reason, "%s: %s", action, failure.Message)

	if failure.Permanent {
		// Returning no error is what keeps this out of the exponential backoff
		// queue: there is nothing to back off from, the same request will fail
		// the same way, and a slow requeue is enough to notice when the world
		// changes.
		return r.finish(ctx, tunnel, ctrl.Result{RequeueAfter: permanentRetryPeriod}, nil)
	}
	return r.finish(ctx, tunnel, ctrl.Result{}, fmt.Errorf("%s: %w", action, err))
}

// finish writes the status and returns, preserving the reconcile's own error.
func (r *TunnelReconciler) finish(
	ctx context.Context,
	tunnel *tunnelv1alpha1.Tunnel,
	result ctrl.Result,
	reconcileErr error,
) (ctrl.Result, error) {
	tunnel.Status.ObservedGeneration = tunnel.Generation
	if err := r.Status().Update(ctx, tunnel); err != nil {
		if reconcileErr != nil {
			// The reconcile's own failure is the one worth reporting: the
			// status write failing on top of it is a symptom, and letting it
			// replace the cause is how the real reason disappears from the log.
			return result, reconcileErr
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return result, reconcileErr
}

func (r *TunnelReconciler) setSynced(t *tunnelv1alpha1.Tunnel, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
		Type:               tunnelv1alpha1.TunnelConditionSynced,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: t.Generation,
	})
}

func (r *TunnelReconciler) setAgentAvailable(t *tunnelv1alpha1.Tunnel, d *appsv1.Deployment) {
	status := metav1.ConditionFalse
	reason := "NoReplicasAvailable"
	msg := "The agent Deployment has no available replica yet."
	if d != nil && d.Status.AvailableReplicas > 0 {
		status = metav1.ConditionTrue
		reason = "AgentRunning"
		msg = "The agent Deployment has a running replica."
	}
	meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
		Type:               tunnelv1alpha1.TunnelConditionAgentAvailable,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: t.Generation,
	})
}

// setReady rolls the other two up, and adds the one thing neither of them can
// see: whether the platform is actually receiving the agent's handshakes.
//
// An agent pod that is Running proves the process started, not that the tunnel
// carries traffic: a wrong credential, or egress that does not let WireGuard
// out, produces a perfectly healthy pod and a tunnel nobody can reach.
func (r *TunnelReconciler) setReady(t *tunnelv1alpha1.Tunnel, onlineStatus string) {
	synced := meta.IsStatusConditionTrue(t.Status.Conditions, tunnelv1alpha1.TunnelConditionSynced)
	agent := meta.IsStatusConditionTrue(t.Status.Conditions, tunnelv1alpha1.TunnelConditionAgentAvailable)

	status, reason, msg := metav1.ConditionFalse, "NotReady", "The tunnel is not ready."
	switch {
	case !synced:
		reason, msg = "NotSynced", "The tunnel is not in sync with the platform."
	case !agent:
		reason, msg = "AgentUnavailable", "The agent is not running."
	case onlineStatus == "online":
		status, reason, msg = metav1.ConditionTrue, "Online", "The tunnel is connected."
	case onlineStatus == "":
		reason, msg = "Unknown", "The platform has not reported a connection state yet."
	default:
		reason = "Offline"
		msg = fmt.Sprintf("The agent is running but the platform reports the tunnel as %q. "+
			"Check that this cluster can open outbound WireGuard traffic.", onlineStatus)
	}

	meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
		Type:               tunnelv1alpha1.TunnelConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: t.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *TunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tunnelv1alpha1.Tunnel{}).
		Owns(&corev1.Secret{}).
		Owns(&appsv1.Deployment{}).
		Named("tunnel").
		Complete(r)
}
