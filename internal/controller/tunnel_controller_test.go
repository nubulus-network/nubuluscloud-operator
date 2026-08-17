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
	"errors"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tunnelv1alpha1 "github.com/nubulus-network/nubuluscloud-operator/api/v1alpha1"
)

var namespaceCounter int

// newNamespace gives each test its own, so that objects left behind by one
// cannot be seen by another.
func newNamespace(t *testing.T) string {
	t.Helper()
	namespaceCounter++
	name := fmt.Sprintf("test-%d", namespaceCounter)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating the namespace: %v", err)
	}
	return name
}

func newCredentialSecret(t *testing.T, namespace string) string {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "nubulus-api", Namespace: namespace},
		Data: map[string][]byte{
			"client_id":     []byte("id"),
			"client_secret": []byte("secret"),
		},
	}
	if err := k8sClient.Create(context.Background(), secret); err != nil {
		t.Fatalf("creating the credential Secret: %v", err)
	}
	return secret.Name
}

func newTunnel(t *testing.T, namespace, name, credential string) *tunnelv1alpha1.Tunnel {
	t.Helper()
	tunnel := &tunnelv1alpha1.Tunnel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: tunnelv1alpha1.TunnelSpec{
			Credentials: tunnelv1alpha1.CredentialsReference{Name: credential},
		},
	}
	if err := k8sClient.Create(context.Background(), tunnel); err != nil {
		t.Fatalf("creating the Tunnel: %v", err)
	}
	return tunnel
}

func newTunnelReconciler(api APIClientFactory) *TunnelReconciler {
	return &TunnelReconciler{
		Client:            k8sClient,
		Scheme:            scheme.Scheme,
		Recorder:          events.NewFakeRecorder(100),
		API:               api,
		DefaultAgentImage: "agent:test",
	}
}

func reconcileTunnel(t *testing.T, r *TunnelReconciler, obj client.Object) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(obj),
	})
	if err != nil {
		t.Fatalf("reconciling: %v", err)
	}
	return res
}

func getTunnel(t *testing.T, ns, name string) *tunnelv1alpha1.Tunnel {
	t.Helper()
	var out tunnelv1alpha1.Tunnel
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &out); err != nil {
		t.Fatalf("reading the Tunnel back: %v", err)
	}
	return &out
}

func TestAFirstReconcileCreatesTheTunnelItsSecretAndItsAgent(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	cred := newCredentialSecret(t, ns)
	api := newFakeAPI(t)

	tunnel := newTunnel(t, ns, tunnelName, cred)
	r := newTunnelReconciler(api.clientFactory())
	reconcileTunnel(t, r, tunnel)

	got := getTunnel(t, ns, tunnelName)
	if got.Status.TunnelID == "" {
		t.Fatal("the status carries no tunnel id")
	}
	if got.Status.CNAMETarget == "" {
		t.Error("the status carries no CNAME target, which is the one thing the user has to publish")
	}
	// The external id is the object's UID, which is what makes a lost create
	// recoverable rather than repeated.
	if got.Status.ExternalID != string(got.UID) {
		t.Errorf("externalID = %q, want the object UID %q", got.Status.ExternalID, got.UID)
	}

	var secret corev1.Secret
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: ns, Name: tokenSecretName(tunnelName),
	}, &secret); err != nil {
		t.Fatalf("reading the credential Secret: %v", err)
	}
	if len(secret.Data[tokenSecretKey]) == 0 {
		t.Error("the credential Secret is empty")
	}

	var deployment appsv1.Deployment
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: ns, Name: agentDeploymentName(tunnelName),
	}, &deployment); err != nil {
		t.Fatalf("reading the agent Deployment: %v", err)
	}
}

// The window this covers is the one the whole external id mechanism exists for:
// the tunnel was created, and the answer never made it into the status. A
// reconciler that simply retried would make a second tunnel holding a second
// address from the pool, with a credential nobody ever saw.
func TestALostCreateIsAdoptedRatherThanRepeated(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	cred := newCredentialSecret(t, ns)
	api := newFakeAPI(t)

	tunnel := newTunnel(t, ns, tunnelName, cred)
	r := newTunnelReconciler(api.clientFactory())
	reconcileTunnel(t, r, tunnel)

	// Wipe the status: this is exactly what a crash between the answer and the
	// status write leaves behind.
	got := getTunnel(t, ns, tunnelName)
	firstID := got.Status.TunnelID
	got.Status.TunnelID = ""
	if err := k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("clearing the status: %v", err)
	}

	reconcileTunnel(t, r, tunnel)

	if n := api.tunnelCount(); n != 1 {
		t.Errorf("the platform holds %d tunnels, want 1: the retry made an orphan", n)
	}
	if api.creates != 2 {
		t.Errorf("creates = %d, want 2: the test did not exercise the retry", api.creates)
	}
	if again := getTunnel(t, ns, tunnelName).Status.TunnelID; again != firstID {
		t.Errorf("tunnelID = %q after adoption, want the original %q", again, firstID)
	}
}

// An adoption comes back without a credential, so the reconciler has to decide
// whether to rotate. Rotating breaks whatever is running with the old one, so
// the answer must be no whenever the stored credential is still there.
func TestAdoptionDoesNotRotateACredentialThatIsStillStored(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	cred := newCredentialSecret(t, ns)
	api := newFakeAPI(t)

	tunnel := newTunnel(t, ns, tunnelName, cred)
	r := newTunnelReconciler(api.clientFactory())
	reconcileTunnel(t, r, tunnel)

	var before corev1.Secret
	key := types.NamespacedName{Namespace: ns, Name: tokenSecretName(tunnelName)}
	if err := k8sClient.Get(ctx, key, &before); err != nil {
		t.Fatalf("reading the credential Secret: %v", err)
	}

	got := getTunnel(t, ns, tunnelName)
	got.Status.TunnelID = ""
	if err := k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("clearing the status: %v", err)
	}
	reconcileTunnel(t, r, tunnel)

	if api.rotations != 0 {
		t.Errorf("rotations = %d, want 0: the running agent was cut off for nothing", api.rotations)
	}
	var after corev1.Secret
	if err := k8sClient.Get(ctx, key, &after); err != nil {
		t.Fatalf("reading the credential Secret: %v", err)
	}
	if string(after.Data[tokenSecretKey]) != string(before.Data[tokenSecretKey]) {
		t.Error("the stored credential changed even though the old one was fine")
	}
}

// The other half of the same decision: with no usable credential there is
// nothing to protect, and rotation is the only way to get one for a tunnel that
// already exists.
func TestAMissingCredentialIsRotatedBack(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	cred := newCredentialSecret(t, ns)
	api := newFakeAPI(t)

	tunnel := newTunnel(t, ns, tunnelName, cred)
	r := newTunnelReconciler(api.clientFactory())
	reconcileTunnel(t, r, tunnel)

	key := types.NamespacedName{Namespace: ns, Name: tokenSecretName(tunnelName)}
	var secret corev1.Secret
	if err := k8sClient.Get(ctx, key, &secret); err != nil {
		t.Fatalf("reading the credential Secret: %v", err)
	}
	if err := k8sClient.Delete(ctx, &secret); err != nil {
		t.Fatalf("deleting the credential Secret: %v", err)
	}

	reconcileTunnel(t, r, tunnel)

	if api.rotations != 1 {
		t.Errorf("rotations = %d, want 1: a tunnel with no stored credential is unusable", api.rotations)
	}
	if err := k8sClient.Get(ctx, key, &secret); err != nil {
		t.Fatalf("the credential Secret was not put back: %v", err)
	}
	if len(secret.Data[tokenSecretKey]) == 0 {
		t.Error("the credential Secret was recreated empty")
	}
}

// Without the finalizer, deleting an object whose create was in flight leaves a
// tunnel that nothing refers to and nobody can find. The finalizer has to be on
// before the first remote write, not after it.
func TestTheFinalizerIsOnBeforeAnythingExistsRemotely(t *testing.T) {
	ns := newNamespace(t)
	cred := newCredentialSecret(t, ns)
	api := newFakeAPI(t)

	tunnel := newTunnel(t, ns, tunnelName, cred)
	r := newTunnelReconciler(api.clientFactory())
	reconcileTunnel(t, r, tunnel)

	got := getTunnel(t, ns, tunnelName)
	found := false
	for _, f := range got.Finalizers {
		if f == tunnelv1alpha1.TunnelFinalizer {
			found = true
		}
	}
	if !found {
		t.Fatalf("no finalizer on the object, got %v", got.Finalizers)
	}
}

func TestDeletingTheObjectDeletesTheTunnel(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	cred := newCredentialSecret(t, ns)
	api := newFakeAPI(t)

	tunnel := newTunnel(t, ns, tunnelName, cred)
	r := newTunnelReconciler(api.clientFactory())
	reconcileTunnel(t, r, tunnel)

	if err := k8sClient.Delete(ctx, getTunnel(t, ns, tunnelName)); err != nil {
		t.Fatalf("deleting the Tunnel: %v", err)
	}
	reconcileTunnel(t, r, tunnel)

	if n := api.tunnelCount(); n != 0 {
		t.Errorf("the platform still holds %d tunnels", n)
	}
	var gone tunnelv1alpha1.Tunnel
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: tunnelName}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the object is still there: the finalizer was not removed (%v)", err)
	}
}

// A create whose answer was lost, followed by a delete, is the worst case: the
// object that could identify the tunnel is about to disappear. Looking it up by
// external id on the way out is what stops that from leaking permanently.
func TestDeletingCleansUpATunnelTheStatusNeverRecorded(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	cred := newCredentialSecret(t, ns)
	api := newFakeAPI(t)

	tunnel := newTunnel(t, ns, tunnelName, cred)
	r := newTunnelReconciler(api.clientFactory())
	reconcileTunnel(t, r, tunnel)

	got := getTunnel(t, ns, tunnelName)
	got.Status.TunnelID = ""
	if err := k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("clearing the status: %v", err)
	}

	if err := k8sClient.Delete(ctx, getTunnel(t, ns, tunnelName)); err != nil {
		t.Fatalf("deleting the Tunnel: %v", err)
	}
	reconcileTunnel(t, r, tunnel)

	if n := api.tunnelCount(); n != 0 {
		t.Errorf("the platform still holds %d tunnels: the unrecorded one leaked", n)
	}
}

// A tunnel removed in the panel has to come back, and the new credential has to
// reach the agent: a Secret written under a pod that is already running changes
// nothing until that pod is replaced.
func TestATunnelDeletedOnThePlatformIsRecreatedAndTheAgentIsRolled(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	cred := newCredentialSecret(t, ns)
	api := newFakeAPI(t)

	tunnel := newTunnel(t, ns, tunnelName, cred)
	r := newTunnelReconciler(api.clientFactory())
	reconcileTunnel(t, r, tunnel)

	first := getTunnel(t, ns, tunnelName).Status.TunnelID
	var deployment appsv1.Deployment
	deployKey := types.NamespacedName{Namespace: ns, Name: agentDeploymentName(tunnelName)}
	if err := k8sClient.Get(ctx, deployKey, &deployment); err != nil {
		t.Fatalf("reading the agent Deployment: %v", err)
	}
	beforeVersion := deployment.Spec.Template.Annotations[credentialVersionAnnotation]

	api.deleteTunnelBehindOurBack(first)
	reconcileTunnel(t, r, tunnel)

	second := getTunnel(t, ns, tunnelName).Status.TunnelID
	if second == "" || second == first {
		t.Fatalf("tunnelID = %q after the platform lost it, want a new one", second)
	}
	if err := k8sClient.Get(ctx, deployKey, &deployment); err != nil {
		t.Fatalf("reading the agent Deployment: %v", err)
	}
	if deployment.Spec.Template.Annotations[credentialVersionAnnotation] == beforeVersion {
		t.Error("the pod template did not change, so the agent would keep using the dead credential")
	}
}

// A credential that cannot be read is the user's to fix. Retrying it in a tight
// loop helps nobody, and the object has to say what is wrong.
func TestAnUnreadableCredentialIsReportedAndNotRetriedHard(t *testing.T) {
	ns := newNamespace(t)
	cred := newCredentialSecret(t, ns)

	tunnel := newTunnel(t, ns, tunnelName, cred)
	r := newTunnelReconciler(failingFactory{err: errors.New("Secret has no client_id")})

	res := reconcileTunnel(t, r, tunnel)
	if res.RequeueAfter == 0 {
		t.Error("a permanent failure must still be requeued slowly, not dropped")
	}

	got := getTunnel(t, ns, tunnelName)
	var synced *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == tunnelv1alpha1.TunnelConditionSynced {
			synced = &got.Status.Conditions[i]
		}
	}
	if synced == nil {
		t.Fatal("no Synced condition on the object")
	}
	if synced.Status != metav1.ConditionFalse || synced.Reason != "CredentialUnreadable" {
		t.Errorf("Synced = %s/%s, want False/CredentialUnreadable", synced.Status, synced.Reason)
	}
}

// A Tunnel naming a Secret that does not exist never reached the platform, so
// there is nothing to clean up and nothing to wait for. Holding it in
// Terminating would mean unsticking a typo by editing finalizers by hand, which
// is a thing users should never have to learn how to do.
func TestATunnelThatNeverReachedThePlatformCanBeDeleted(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)

	tunnel := newTunnel(t, ns, tunnelName, "no-existe")
	r := newTunnelReconciler(failingFactory{err: errors.New("no such Secret")})
	reconcileTunnel(t, r, tunnel)

	if err := k8sClient.Delete(ctx, getTunnel(t, ns, tunnelName)); err != nil {
		t.Fatalf("deleting the Tunnel: %v", err)
	}
	reconcileTunnel(t, r, tunnel)

	var gone tunnelv1alpha1.Tunnel
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: tunnelName}, &gone)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the object is stuck in Terminating over a credential it never used: %v", err)
	}
}

// The other side of the same decision. Once something may exist on the
// platform, an unreadable credential must NOT let the object go: the finalizer
// is the only remaining pointer to what has to be cleaned up.
func TestATunnelThatMayExistIsHeldWhenItsCredentialBreaks(t *testing.T) {
	ctx := context.Background()
	ns := newNamespace(t)
	cred := newCredentialSecret(t, ns)
	api := newFakeAPI(t)

	tunnel := newTunnel(t, ns, tunnelName, cred)
	reconcileTunnel(t, newTunnelReconciler(api.clientFactory()), tunnel)

	if err := k8sClient.Delete(ctx, getTunnel(t, ns, tunnelName)); err != nil {
		t.Fatalf("deleting the Tunnel: %v", err)
	}
	broken := newTunnelReconciler(failingFactory{err: errors.New("credential gone")})
	reconcileTunnel(t, broken, tunnel)

	var held tunnelv1alpha1.Tunnel
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: ns, Name: tunnelName,
	}, &held); err != nil {
		t.Fatalf("the object was let go while its tunnel is still on the platform: %v", err)
	}
	if api.tunnelCount() != 1 {
		t.Errorf("the platform holds %d tunnels, want 1", api.tunnelCount())
	}
}
