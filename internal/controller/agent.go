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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	tunnelv1alpha1 "github.com/Nubulus-Network/nubuluscloud-operator/api/v1alpha1"
)

const (
	// tokenSecretKey is the key the tunnel credential is written under. It is
	// also the environment variable the agent reads, which is why the two are
	// the same string and not two constants that could drift.
	tokenSecretKey = "TUNNEL_TOKEN"

	// metricsPort and statusPort are where the agent is told to serve. Its own
	// defaults bind to loopback, which inside a pod means nothing outside the
	// container can reach them, including the kubelet running the probes. So
	// they are always set explicitly here.
	metricsPort = 9990
	statusPort  = 9999

	// credentialVersionAnnotation carries the resourceVersion of the Secret
	// holding the token, which is what makes a rotated credential actually
	// reach the running agent.
	//
	// A Secret consumed through secretKeyRef becomes an environment variable,
	// and an environment variable is fixed for the life of the process: writing
	// a new token into the Secret changes NOTHING until the pod is replaced.
	// Putting the version in the pod template is what turns a rotation into a
	// rollout. The resourceVersion is used rather than a hash of the token
	// because it changes just as reliably and reveals nothing.
	credentialVersionAnnotation = "tunnel.nubulusnetwork.es/credential-version"
)

// agentDeploymentName is the Deployment that runs the client end of a tunnel.
func agentDeploymentName(tunnelName string) string {
	return tunnelName + "-agent"
}

// tokenSecretName is the Secret the tunnel's credential is written to.
func tokenSecretName(tunnelName string) string {
	return tunnelName + "-tunnel-token"
}

// agentLabels are the labels every object the operator creates for a tunnel
// carries, and the selector of its Deployment.
func agentLabels(tunnel *tunnelv1alpha1.Tunnel) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "nubulus-tunnel-agent",
		"app.kubernetes.io/instance":   tunnel.Name,
		"app.kubernetes.io/managed-by": "nubuluscloud-operator",
	}
}

// buildAgentDeployment is the desired Deployment for a tunnel's agent.
//
// credentialVersion is the resourceVersion of the Secret holding the token; see
// credentialVersionAnnotation for why it is on the pod template.
func buildAgentDeployment(
	tunnel *tunnelv1alpha1.Tunnel,
	defaultImage string,
	credentialVersion string,
) *appsv1.Deployment {
	labels := agentLabels(tunnel)

	image := tunnel.Spec.Agent.Image
	if image == "" {
		image = defaultImage
	}

	logLevel := tunnel.Spec.Agent.LogLevel
	if logLevel == "" {
		logLevel = "info"
	}

	env := []corev1.EnvVar{{
		Name: tokenSecretKey,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: tokenSecretName(tunnel.Name),
				},
				Key: tokenSecretKey,
			},
		},
	}, {
		Name:  "LOG_LEVEL",
		Value: logLevel,
	}, {
		Name:  "LOG_FORMAT",
		Value: "json",
	}, {
		// Both listeners default to loopback. See metricsPort.
		Name:  "METRICS_LISTEN_ADDR",
		Value: "0.0.0.0:9990",
	}, {
		Name:  "STATUS_LISTEN_ADDR",
		Value: "0.0.0.0:9999",
	}}

	// The agent fetches its own configuration from a public route of the API,
	// with the tunnel token, a different endpoint and a different credential
	// from the one the operator uses. It is only overridden when the whole
	// platform is being pointed somewhere else.
	if tunnel.Spec.Endpoints != nil && tunnel.Spec.Endpoints.API != "" {
		env = append(env, corev1.EnvVar{
			Name:  "API_URL",
			Value: strings.TrimSuffix(tunnel.Spec.Endpoints.API, "/") + "/api/v2/tunnel-config/",
		})
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentDeploymentName(tunnel.Name),
			Namespace: tunnel.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			// ONE. A tunnel is a single WireGuard peer holding one address, so
			// a second replica does not share the load: it uses the same key
			// from a different pod and the two take turns owning the session.
			// Recreate rather than RollingUpdate for the same reason: the
			// default would deliberately run two of them during every rollout.
			Replicas: ptr.To[int32](1),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						credentialVersionAnnotation: credentialVersion,
					},
				},
				Spec: corev1.PodSpec{
					ImagePullSecrets: tunnel.Spec.Agent.ImagePullSecrets,
					NodeSelector:     tunnel.Spec.Agent.NodeSelector,
					Tolerations:      tunnel.Spec.Agent.Tolerations,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						// The image is built FROM scratch and declares no user,
						// so without a uid here the kubelet refuses to start it
						// under runAsNonRoot. Any uid works: the binary is
						// static and the filesystem is read-only.
						RunAsUser:      ptr.To[int64](65532),
						RunAsGroup:     ptr.To[int64](65532),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            "agent",
						Image:           image,
						ImagePullPolicy: tunnel.Spec.Agent.ImagePullPolicy,
						Env:             env,
						Resources:       tunnel.Spec.Agent.Resources,
						Ports: []corev1.ContainerPort{{
							Name:          "metrics",
							ContainerPort: metricsPort,
						}, {
							Name:          "status",
							ContainerPort: statusPort,
						}},
						// WireGuard here is a userspace implementation, so the
						// container needs none of what a tunnel usually asks
						// for: no NET_ADMIN, no privileged, no kernel module.
						// This block is what keeps that true.
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Privileged:               ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						// /status answers 200 whenever the process is serving,
						// whatever the tunnel is doing, so this says the agent
						// is alive and NOT that traffic is flowing. Whether the
						// tunnel is actually connected is onlineStatus, which
						// comes from the platform.
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/status",
									Port: intstr.FromString("status"),
								},
							},
							InitialDelaySeconds: 10,
							PeriodSeconds:       30,
							TimeoutSeconds:      5,
							FailureThreshold:    3,
						},
					}},
				},
			},
		},
	}
}
