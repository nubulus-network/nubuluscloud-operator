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
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tunnelv1alpha1 "github.com/Nubulus-Network/nubuluscloud-operator/api/v1alpha1"
	"github.com/Nubulus-Network/nubuluscloud-operator/internal/nubulus"
)

// APIClientFactory hands out platform clients built from a Tunnel's credential.
//
// It is an interface so the controllers can be tested against an httptest
// server without an identity provider anywhere near them.
type APIClientFactory interface {
	// ClientFor builds the client for a tunnel, reading its credential from the
	// Secret named in its spec, in the tunnel's OWN namespace, never anywhere
	// else. That restriction is the tenancy model: an account is reachable only
	// from namespaces holding a credential for it.
	ClientFor(ctx context.Context, k8s client.Client, tunnel *tunnelv1alpha1.Tunnel) (*nubulus.Client, error)
}

// CachedClientFactory builds real clients and keeps them.
//
// The caching is not an optimisation, it is a correctness-adjacent necessity: a
// fresh client mints a fresh OAuth2 token source, and a fresh token source
// fetches a token on its first request. Building one per reconcile would mean a
// call to the token endpoint per reconcile of every object forever, for a
// credential that is good for twelve hours.
//
// The cache key includes the Secret's resourceVersion, so a rotated credential
// produces a new client on the next reconcile rather than an old one that keeps
// working until the operator restarts.
type CachedClientFactory struct {
	// UserAgent is sent on every request.
	UserAgent string

	mu      sync.Mutex
	clients map[string]*nubulus.Client
}

// ClientFor implements APIClientFactory.
func (f *CachedClientFactory) ClientFor(
	ctx context.Context,
	k8s client.Client,
	tunnel *tunnelv1alpha1.Tunnel,
) (*nubulus.Client, error) {
	cfg, key, err := configFor(ctx, k8s, tunnel, f.UserAgent)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.clients == nil {
		f.clients = map[string]*nubulus.Client{}
	}
	if cached, ok := f.clients[key]; ok {
		return cached, nil
	}

	// The context handed in is a reconcile's, and it is cancelled the moment
	// that reconcile returns. nubulus.New drops the cancellation from the one
	// it keeps for token fetches; see the comment there, which is the whole
	// reason this is safe to cache.
	built, err := nubulus.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	f.clients[key] = built
	return built, nil
}

// configFor reads the credential and turns a Tunnel into a client config,
// returning the cache key alongside it.
func configFor(
	ctx context.Context,
	k8s client.Client,
	tunnel *tunnelv1alpha1.Tunnel,
	userAgent string,
) (nubulus.Config, string, error) {
	ref := tunnel.Spec.Credentials

	idKey := ref.ClientIDKey
	if idKey == "" {
		idKey = "client_id"
	}
	secretKey := ref.ClientSecretKey
	if secretKey == "" {
		secretKey = "client_secret"
	}

	var secret corev1.Secret
	nn := types.NamespacedName{Namespace: tunnel.Namespace, Name: ref.Name}
	if err := k8s.Get(ctx, nn, &secret); err != nil {
		return nubulus.Config{}, "", fmt.Errorf("reading the credential Secret %s: %w", nn, err)
	}

	clientID := string(secret.Data[idKey])
	clientSecret := string(secret.Data[secretKey])
	// Named separately rather than as "the Secret is wrong", because the two
	// have different fixes and the key names are configurable.
	if clientID == "" {
		return nubulus.Config{}, "", fmt.Errorf("Secret %s has no %q", nn, idKey)
	}
	if clientSecret == "" {
		return nubulus.Config{}, "", fmt.Errorf("Secret %s has no %q", nn, secretKey)
	}

	cfg := nubulus.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		UserAgent:    userAgent,
	}
	if e := tunnel.Spec.Endpoints; e != nil {
		cfg.TokenURL = e.TokenURL
		cfg.TunnelEndpoint = e.API
		cfg.ProjectID = e.ProjectID
	}

	// The key covers everything that changes the client's behaviour. The
	// resourceVersion stands in for the credential itself: comparing the secret
	// values would work too and would mean holding them in a map key.
	key := fmt.Sprintf("%s/%s@%s|%s|%s|%s",
		nn.Namespace, nn.Name, secret.ResourceVersion,
		cfg.TokenURL, cfg.TunnelEndpoint, cfg.ProjectID)

	return cfg, key, nil
}
