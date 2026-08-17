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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	tunnelv1alpha1 "github.com/nubulus-network/nubuluscloud-operator/api/v1alpha1"
	"github.com/nubulus-network/nubuluscloud-operator/internal/nubulus"
)

// fakeAPI is a stand-in for the tunnel API, reproducing the behaviours the
// reconcilers depend on rather than a general-purpose mock.
//
// Four of them are the reason it exists at all, and none can be exercised
// without a server that behaves like this one:
//
//   - a create carrying an external id already present ADOPTS, answering 200
//     with adopted set and WITHOUT a credential;
//   - a credential is issued exactly once, so a tunnel that already exists can
//     only get one through a rotation;
//   - a route is always created enabled, and a priority of 0 on create becomes
//     100;
//   - a read of a tunnel does not carry the credential at all.
type fakeAPI struct {
	mu sync.Mutex

	tunnels map[string]*fakeTunnel
	routes  map[string][]*nubulus.Route

	nextID int

	// creates counts POSTs to the tunnel collection, including the ones that
	// only adopt. It is how a test proves that a retry did not make a second
	// tunnel: the count goes up, the number of tunnels does not.
	creates int
	// rotations counts credentials issued for an existing tunnel. A test that
	// asserts the agent was not disturbed asserts on this.
	rotations int

	server *httptest.Server
}

type fakeTunnel struct {
	tunnel nubulus.Tunnel
	token  string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		tunnels: map[string]*fakeTunnel{},
		routes:  map[string][]*nubulus.Route{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(f.server.Close)
	return f
}

// clientFactory adapts the fake into what the reconcilers take. The credential
// is not checked: what is being tested is the reconcilers, and an identity
// provider in the loop would only add a second thing that can fail.
func (f *fakeAPI) clientFactory() APIClientFactory {
	return fakeFactory{endpoint: f.server.URL}
}

type fakeFactory struct{ endpoint string }

func (f fakeFactory) ClientFor(
	ctx context.Context, _ client.Client, _ *tunnelv1alpha1.Tunnel,
) (*nubulus.Client, error) {
	return nubulus.New(ctx, nubulus.Config{
		TunnelEndpoint: f.endpoint,
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
	})
}

// failingFactory stands in for a credential that cannot be read.
type failingFactory struct{ err error }

func (f failingFactory) ClientFor(
	context.Context, client.Client, *tunnelv1alpha1.Tunnel,
) (*nubulus.Client, error) {
	return nil, f.err
}

func (f *fakeAPI) tunnelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tunnels)
}

func (f *fakeAPI) routeCount(tunnelID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.routes[tunnelID])
}

func (f *fakeAPI) routesOf(tunnelID string) []nubulus.Route {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]nubulus.Route, 0, len(f.routes[tunnelID]))
	for _, r := range f.routes[tunnelID] {
		out = append(out, *r)
	}
	return out
}

// deleteTunnelBehindOurBack simulates somebody removing a tunnel in the panel.
func (f *fakeAPI) deleteTunnelBehindOurBack(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tunnels, id)
	delete(f.routes, id)
}

func (f *fakeAPI) route(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := strings.TrimPrefix(r.URL.Path, "/api/v2/tunnels")
	path = strings.Trim(path, "/")

	switch {
	case path == "" && r.Method == http.MethodPost:
		f.createTunnel(w, r)
	case path == "" && r.Method == http.MethodGet:
		f.listTunnels(w, r)
	case strings.HasSuffix(path, "/rotate-token") && r.Method == http.MethodPost:
		f.rotateToken(w, strings.TrimSuffix(path, "/rotate-token"))
	case strings.HasSuffix(path, "/routes") && r.Method == http.MethodPost:
		f.createRoute(w, r, strings.TrimSuffix(path, "/routes"))
	case strings.HasSuffix(path, "/routes") && r.Method == http.MethodGet:
		f.listRoutes(w, strings.TrimSuffix(path, "/routes"))
	case strings.Contains(path, "/routes/") && r.Method == http.MethodPut:
		id, routeID := splitRoutePath(path)
		f.updateRoute(w, r, id, routeID)
	case strings.Contains(path, "/routes/") && r.Method == http.MethodDelete:
		id, routeID := splitRoutePath(path)
		f.deleteRoute(w, id, routeID)
	case r.Method == http.MethodGet:
		f.getTunnel(w, path)
	case r.Method == http.MethodDelete:
		delete(f.tunnels, path)
		delete(f.routes, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "no such route in the fake")
	}
}

func splitRoutePath(path string) (tunnelID, routeID string) {
	parts := strings.SplitN(path, "/routes/", 2)
	return parts[0], parts[1]
}

func (f *fakeAPI) createTunnel(w http.ResponseWriter, r *http.Request) {
	f.creates++

	var in nubulus.CreateTunnelInput
	_ = json.NewDecoder(r.Body).Decode(&in)

	if in.ExternalID != "" {
		for _, existing := range f.tunnels {
			if existing.tunnel.ExternalID != in.ExternalID {
				continue
			}
			// Adoption. Note what is NOT in this answer: no token, no
			// WireGuard block. A create that handed the credential back would
			// be a way of reading a secret that is only ever issued once.
			writeJSON(w, http.StatusOK, nubulus.CreateTunnelResult{
				TunnelID:        existing.tunnel.ID,
				TunnelSubdomain: existing.tunnel.TunnelSubdomain,
				CNAMETarget:     existing.tunnel.TunnelSubdomain,
				WireGuardIP:     existing.tunnel.WireGuardIP,
				Adopted:         true,
			})
			return
		}
	}

	f.nextID++
	id := fmt.Sprintf("tun-%d", f.nextID)
	token := fmt.Sprintf("tuntok-%d", f.nextID)
	subdomain := id + ".tunnels.example"

	f.tunnels[id] = &fakeTunnel{
		tunnel: nubulus.Tunnel{
			ID:              id,
			Name:            in.Name,
			ExternalID:      in.ExternalID,
			TunnelSubdomain: subdomain,
			WireGuardIP:     fmt.Sprintf("10.10.0.%d", f.nextID),
			Status:          "active",
			OnlineStatus:    "offline",
		},
		token: token,
	}

	writeJSON(w, http.StatusCreated, nubulus.CreateTunnelResult{
		TunnelID:        id,
		TunnelToken:     token,
		TunnelSubdomain: subdomain,
		CNAMETarget:     subdomain,
		WireGuardIP:     f.tunnels[id].tunnel.WireGuardIP,
	})
}

func (f *fakeAPI) listTunnels(w http.ResponseWriter, r *http.Request) {
	wanted := r.URL.Query().Get("external_id")
	data := []nubulus.TunnelSummary{}
	for _, t := range f.tunnels {
		if wanted != "" && t.tunnel.ExternalID != wanted {
			continue
		}
		data = append(data, nubulus.TunnelSummary{Tunnel: t.tunnel})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "limit": 100, "offset": 0})
}

func (f *fakeAPI) getTunnel(w http.ResponseWriter, id string) {
	t, ok := f.tunnels[id]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "no such tunnel")
		return
	}
	// A read never carries the credential.
	writeJSON(w, http.StatusOK, nubulus.TunnelWithRoutes{
		Tunnel: &t.tunnel,
		Routes: derefRoutes(f.routes[id]),
	})
}

func (f *fakeAPI) rotateToken(w http.ResponseWriter, id string) {
	t, ok := f.tunnels[id]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "no such tunnel")
		return
	}
	f.rotations++
	t.token = fmt.Sprintf("%s-rotated-%d", t.token, f.rotations)
	writeJSON(w, http.StatusOK, nubulus.RotateTokenResult{TunnelID: id, TunnelToken: t.token})
}

func (f *fakeAPI) createRoute(w http.ResponseWriter, r *http.Request, tunnelID string) {
	if _, ok := f.tunnels[tunnelID]; !ok {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "no such tunnel")
		return
	}

	var in nubulus.CreateRouteInput
	_ = json.NewDecoder(r.Body).Decode(&in)

	f.nextID++
	route := &nubulus.Route{
		ID:             fmt.Sprintf("rt-%d", f.nextID),
		TunnelID:       tunnelID,
		Type:           in.Type,
		Hostname:       in.Hostname,
		PathPrefix:     in.PathPrefix,
		UpstreamHost:   in.UpstreamHost,
		UpstreamPort:   in.UpstreamPort,
		UpstreamScheme: in.UpstreamScheme,
		StripPrefix:    in.StripPrefix,
		Priority:       in.Priority,
		// Always. There is no `enabled` in the create body, so a route that
		// has to end up disabled needs the update that follows.
		Enabled: true,
	}
	if route.PathPrefix == "" {
		route.PathPrefix = "/"
	}
	if route.UpstreamScheme == "" {
		route.UpstreamScheme = "http"
	}
	// A priority of 0 is read as unset, because it arrives as a value and not
	// as a pointer. The update can set 0; the create cannot.
	if route.Priority == 0 {
		route.Priority = 100
	}

	f.routes[tunnelID] = append(f.routes[tunnelID], route)
	writeJSON(w, http.StatusCreated, route)
}

func (f *fakeAPI) listRoutes(w http.ResponseWriter, tunnelID string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"routes": derefRoutes(f.routes[tunnelID]),
		"total":  len(f.routes[tunnelID]),
	})
}

func (f *fakeAPI) updateRoute(w http.ResponseWriter, r *http.Request, tunnelID, routeID string) {
	var in nubulus.UpdateRouteInput
	_ = json.NewDecoder(r.Body).Decode(&in)

	for _, route := range f.routes[tunnelID] {
		if route.ID != routeID {
			continue
		}
		if in.UpstreamHost != nil {
			route.UpstreamHost = *in.UpstreamHost
		}
		if in.UpstreamPort != nil {
			route.UpstreamPort = *in.UpstreamPort
		}
		if in.UpstreamScheme != nil {
			route.UpstreamScheme = *in.UpstreamScheme
		}
		if in.StripPrefix != nil {
			route.StripPrefix = *in.StripPrefix
		}
		if in.Priority != nil {
			route.Priority = *in.Priority
		}
		if in.Enabled != nil {
			route.Enabled = *in.Enabled
		}
		writeJSON(w, http.StatusOK, route)
		return
	}
	writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "no such route")
}

func (f *fakeAPI) deleteRoute(w http.ResponseWriter, tunnelID, routeID string) {
	kept := make([]*nubulus.Route, 0, len(f.routes[tunnelID]))
	for _, route := range f.routes[tunnelID] {
		if route.ID != routeID {
			kept = append(kept, route)
		}
	}
	f.routes[tunnelID] = kept
	w.WriteHeader(http.StatusNoContent)
}

func derefRoutes(in []*nubulus.Route) []nubulus.Route {
	out := make([]nubulus.Route, 0, len(in))
	for _, r := range in {
		out = append(out, *r)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
