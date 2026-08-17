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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	tunnelv1alpha1 "github.com/nubulus-network/nubuluscloud-operator/api/v1alpha1"
)

func testTunnel() *tunnelv1alpha1.Tunnel {
	return &tunnelv1alpha1.Tunnel{
		ObjectMeta: metav1.ObjectMeta{Name: "produccion", Namespace: "apps"},
	}
}

func envOf(d *appsv1.Deployment) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e
	}
	return out
}

// The agent's own defaults bind both listeners to loopback, which inside a pod
// means the kubelet cannot reach them: the liveness probe would fail forever on
// a perfectly healthy agent. This is the test that keeps someone from tidying
// those two variables away as redundant.
func TestTheAgentIsToldToListenOnAllInterfaces(t *testing.T) {
	d := buildAgentDeployment(testTunnel(), "agent:v1", "42")
	env := envOf(d)

	for _, name := range []string{"METRICS_LISTEN_ADDR", "STATUS_LISTEN_ADDR"} {
		v, ok := env[name]
		if !ok {
			t.Errorf("%s is not set; the agent would bind to loopback and the probes could not reach it", name)
			continue
		}
		if v.Value != "0.0.0.0:9990" && v.Value != "0.0.0.0:9999" {
			t.Errorf("%s = %q, which is not reachable from outside the container", name, v.Value)
		}
	}
}

// A Secret consumed as an environment variable is fixed for the life of the
// process, so writing a new token changes nothing until the pod is replaced.
// The annotation is what makes a rotation reach the running agent.
func TestARotatedCredentialChangesThePodTemplate(t *testing.T) {
	before := buildAgentDeployment(testTunnel(), "agent:v1", "42")
	after := buildAgentDeployment(testTunnel(), "agent:v1", "43")

	got := before.Spec.Template.Annotations[credentialVersionAnnotation]
	if got != "42" {
		t.Fatalf("the credential version is not on the pod template, got %q", got)
	}
	if after.Spec.Template.Annotations[credentialVersionAnnotation] == got {
		t.Error("a new credential version must change the pod template, or the agent keeps the old token")
	}
}

// A tunnel is one WireGuard peer holding one address. Two agents with the same
// credential do not share the load, they take turns owning the session, so both
// the replica count and the strategy matter: RollingUpdate would deliberately
// run two of them during every rollout.
func TestOnlyOneAgentEverRuns(t *testing.T) {
	d := buildAgentDeployment(testTunnel(), "agent:v1", "42")

	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", d.Spec.Replicas)
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("strategy = %q, want Recreate", d.Spec.Strategy.Type)
	}
}

// The whole reason this runs as an ordinary Deployment is that WireGuard here
// is userspace. If anybody ever "fixes" a connection problem by adding
// NET_ADMIN or privileged, that is a different product and this should stop
// them.
func TestTheAgentAsksForNoPrivileges(t *testing.T) {
	d := buildAgentDeployment(testTunnel(), "agent:v1", "42")
	c := d.Spec.Template.Spec.Containers[0]

	if c.SecurityContext == nil {
		t.Fatal("the agent container has no security context")
	}
	if c.SecurityContext.Privileged == nil || *c.SecurityContext.Privileged {
		t.Error("the agent must not be privileged")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("the agent must not allow privilege escalation")
	}
	if c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Add) != 0 {
		t.Errorf("the agent must add no capabilities, got %v", c.SecurityContext.Capabilities)
	}

	pod := d.Spec.Template.Spec.SecurityContext
	if pod == nil || pod.RunAsNonRoot == nil || !*pod.RunAsNonRoot {
		t.Error("the agent must run as non-root")
	}
	// The image is FROM scratch and declares no user, so runAsNonRoot without
	// an explicit uid makes the kubelet refuse to start it.
	if pod == nil || pod.RunAsUser == nil || *pod.RunAsUser == 0 {
		t.Error("runAsNonRoot needs an explicit non-zero uid: the image declares none")
	}
}

func TestTheTunnelSpecCanOverrideTheAgentImage(t *testing.T) {
	tunnel := testTunnel()
	tunnel.Spec.Agent.Image = "mirror.example.com/tunnel:v3"

	d := buildAgentDeployment(tunnel, "agent:v1", "42")
	if got := d.Spec.Template.Spec.Containers[0].Image; got != "mirror.example.com/tunnel:v3" {
		t.Errorf("image = %q, want the one from the spec", got)
	}
}
