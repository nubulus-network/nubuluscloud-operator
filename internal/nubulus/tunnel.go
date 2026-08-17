package nubulus

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// TunnelClient is the tunnel API. Like the DNS one, every route it uses is
// scoped to the account behind the token, so nothing here takes an account id.
//
// Only the v2 surface is modelled. There is an older one in which a tunnel *is*
// a single destination, and v2 replaces it with a tunnel plus a set of routes;
// the two are incompatible models of the same object, and a client that spoke
// both would let a caller mix them.
type TunnelClient struct {
	service
}

// Tunnel is a tunnel as a read returns it.
//
// THE CREATE ANSWERS A DIFFERENT SHAPE: see CreateTunnelResult. That is the
// first thing to get wrong here: the field holding the identifier is `id` on a
// read and `tunnel_id` on the create, and two of the values that matter most
// exist only in the create.
//
// The fields of the superseded model (a single external domain and target) are
// deliberately not mapped: they are still in the row, and in v2 they mean
// nothing.
type Tunnel struct {
	ID string `json:"id"`
	// AccountID is who the tunnel belongs to.
	AccountID string `json:"account_id"`
	// UserID is the identity that created it. It is audit data: ownership is
	// the account, and a tunnel created with an application token carries the
	// subject of that token rather than of any person.
	UserID string `json:"user_id"`

	// Name is the label chosen at creation, and ExternalID the caller's own
	// identifier for this tunnel. Both are omitted when they were never set,
	// which is the case for every tunnel made before they existed.
	Name       string `json:"name,omitempty"`
	ExternalID string `json:"external_id,omitempty"`

	// TunnelSubdomain is the name the platform serves this tunnel on, and
	// CNAMETarget in the create answer is what a customer hostname points at.
	TunnelSubdomain string `json:"tunnel_subdomain"`

	// WireGuardIP is the address assigned to the client end of the tunnel.
	WireGuardIP string `json:"wireguard_ip"`
	// WireGuardPublicKey is the client's public key. Its private half is in the
	// create answer and nowhere else.
	WireGuardPublicKey string `json:"wireguard_public_key,omitempty"`

	// Status is the lifecycle of the tunnel; routes may only be written into an
	// active one.
	Status string `json:"status"`
	// OnlineStatus is whether the client end is currently connected, derived
	// from the last handshake. It changes on its own, with nothing applied.
	OnlineStatus    string     `json:"online_status"`
	LastHandshakeAt *time.Time `json:"last_handshake_at,omitempty"`
	StatusChangedAt *time.Time `json:"status_changed_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TunnelSummary is what the listing returns: a tunnel plus how many routes hang
// off it. The route count is the only field a read of a single tunnel does not
// have: there the routes themselves come back instead.
type TunnelSummary struct {
	Tunnel
	RouteCount int `json:"route_count"`
}

// TunnelWithRoutes is a read of one tunnel: the tunnel, and its routes.
type TunnelWithRoutes struct {
	Tunnel *Tunnel `json:"tunnel"`
	Routes []Route `json:"routes"`
}

// CreateTunnelResult is the answer to a create, and it is NOT a Tunnel.
//
// Two of its fields exist only here and can never be read back:
//
//   - TunnelToken, the credential the tunnel client authenticates with;
//   - WireGuard.Interface.PrivateKey, the client end of the key pair.
//
// A read omits both, on purpose: the platform does not hand a secret out
// twice. Anything that keeps them has to carry the value it already has
// forward, because refreshing from the API would write empty strings over them,
// and importing a tunnel cannot recover them at all.
type CreateTunnelResult struct {
	// TunnelID is the identifier. A read calls the same value `id`.
	TunnelID string `json:"tunnel_id"`

	// TunnelToken is shown once. Treat it as a secret.
	//
	// EMPTY WHEN Adopted IS TRUE. See Adopted.
	TunnelToken string `json:"tunnel_token"`

	TunnelSubdomain string `json:"tunnel_subdomain"`
	// CNAMETarget is what a customer hostname is pointed at.
	CNAMETarget string `json:"cname_target"`
	// Instructions is prose meant for a human setting the client up.
	Instructions string `json:"instructions"`

	// WireGuard is EMPTY WHEN Adopted IS TRUE, for the same reason as
	// TunnelToken: its Interface.PrivateKey is a credential.
	WireGuard   WireGuardConfig `json:"wireguard"`
	WireGuardIP string          `json:"wireguard_ip"`

	// Adopted says the create made nothing: a tunnel with this ExternalID was
	// already there and this is it. The API answers 200 rather than 201.
	//
	// The credentials are deliberately absent in that case. A create that
	// handed them back would be a way of reading a secret that is only ever
	// issued once. So an adopted result identifies a tunnel and cannot run
	// one: whatever wants to use it has to rotate the credential first.
	Adopted bool `json:"adopted"`
}

// CreateTunnelInput is the optional body of a create.
//
// Both fields are optional and so is the whole thing; a zero value produces
// exactly the request the API accepted before either existed.
type CreateTunnelInput struct {
	// Name is a label. Cosmetic, not unique, and nothing keys on it.
	Name string `json:"name,omitempty"`

	// ExternalID is the caller's own identifier for this tunnel, unique within
	// the account. It is what makes a create recoverable: repeating one with
	// the same ExternalID adopts the existing tunnel instead of making a
	// second, which is the difference between a lost answer costing a retry
	// and it costing an orphan nobody can reach.
	ExternalID string `json:"external_id,omitempty"`
}

// RotateTokenResult is a freshly issued credential for an existing tunnel.
type RotateTokenResult struct {
	TunnelID    string `json:"tunnel_id"`
	TunnelToken string `json:"tunnel_token"`
}

// WireGuardConfig is the client end of the tunnel, ready to be written to a
// configuration file.
type WireGuardConfig struct {
	Interface WireGuardInterface `json:"interface"`
	Peer      WireGuardPeer      `json:"peer"`
}

// WireGuardInterface is the local side. PrivateKey is only ever populated in
// the answer to a create.
type WireGuardInterface struct {
	PrivateKey string `json:"private_key"`
	Address    string `json:"address"`
	DNS        string `json:"dns"`
}

// WireGuardPeer is the remote side.
type WireGuardPeer struct {
	PublicKey           string `json:"public_key"`
	Endpoint            string `json:"endpoint"`
	AllowedIPs          string `json:"allowed_ips"`
	PersistentKeepalive int    `json:"persistent_keepalive"`
}

// Route sends requests for a hostname, optionally only under a path, to an
// upstream reachable from the machine the tunnel client runs on.
type Route struct {
	ID       string `json:"id"`
	TunnelID string `json:"tunnel_id"`

	// Type is "host" or "path", and Hostname is an FQDN. PathPrefix is "/" for
	// a host route and is required, and must not be "/", for a path one.
	Type       string `json:"type"`
	Hostname   string `json:"hostname"`
	PathPrefix string `json:"path_prefix"`

	UpstreamHost   string `json:"upstream_host"`
	UpstreamPort   int    `json:"upstream_port"`
	UpstreamScheme string `json:"upstream_scheme"`
	// StripPrefix removes PathPrefix before the request reaches the upstream.
	StripPrefix bool `json:"strip_prefix"`

	Enabled bool `json:"enabled"`
	// Priority orders the routes of a tunnel: lower wins.
	Priority int `json:"priority"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateRouteInput is the body of a create.
//
// It has no `enabled` field and that is not an omission: a route is created
// enabled, always, and only an update can disable one. Anything that has to end
// up with a disabled route therefore creates it and then updates it, in two
// calls.
//
// Priority carries a second trap: the create reads 0 as "unset" and turns it
// into the default of 100, while an update can genuinely set it to 0 because
// there it arrives as a pointer. Sending the intended value explicitly on
// create (never 0 to mean 0) is what keeps the two consistent.
type CreateRouteInput struct {
	Type       string `json:"type"`
	Hostname   string `json:"hostname"`
	PathPrefix string `json:"path_prefix,omitempty"`

	UpstreamHost   string `json:"upstream_host"`
	UpstreamPort   int    `json:"upstream_port"`
	UpstreamScheme string `json:"upstream_scheme,omitempty"`
	StripPrefix    bool   `json:"strip_prefix"`

	Priority int `json:"priority,omitempty"`
}

// UpdateRouteInput is the body of an update: every field optional, and a nil
// one left exactly as it is.
//
// WHAT IS NOT HERE CANNOT BE UPDATED. Type, Hostname and PathPrefix are fixed
// for the life of a route, so anything modelling them as changeable would plan
// a change that the apply then silently does not make, which is worse than
// refusing it.
type UpdateRouteInput struct {
	UpstreamHost   *string `json:"upstream_host,omitempty"`
	UpstreamPort   *int    `json:"upstream_port,omitempty"`
	UpstreamScheme *string `json:"upstream_scheme,omitempty"`
	StripPrefix    *bool   `json:"strip_prefix,omitempty"`
	Priority       *int    `json:"priority,omitempty"`
	Enabled        *bool   `json:"enabled,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Tunnels
// ─────────────────────────────────────────────────────────────────────────────

// tunnelPageSize is how many tunnels a page of the listing asks for. The API's
// own default is smaller; asking for more keeps the number of round trips down
// without assuming the ceiling is honoured. See ListTunnels.
const tunnelPageSize = 100

// listPageLimit stops the pagination loop from running forever against a server
// that ignores the offset and keeps answering the same page. It is far above
// any real number of tunnels, so reaching it means something is wrong rather
// than that somebody has a lot of them.
const listPageLimit = 1000

// CreateTunnel creates a tunnel, or adopts the one already carrying the same
// ExternalID.
//
// Nothing about the tunnel is chosen by the caller beyond those two labels: the
// address, the key pair and the credential are all assigned by the platform,
// and two of those come back exactly once.
//
// WITH AN ExternalID THIS IS RECOVERABLE, WITHOUT ONE IT IS NOT. A create whose
// answer is lost (the process died, the connection dropped) has left a tunnel
// behind either way. Repeating it with the same ExternalID finds that tunnel
// and returns it with Adopted set; repeating it without one makes a second,
// which holds an address from the pool and a credential nobody ever saw.
//
// An adopted result carries no credentials. Deciding what to do about that is
// the caller's: see Adopted.
func (c *TunnelClient) CreateTunnel(ctx context.Context, in CreateTunnelInput) (*CreateTunnelResult, error) {
	var out CreateTunnelResult
	// The body is always sent, even when both fields are empty. `omitempty`
	// reduces that to `{}`, which the API treats exactly as it treats no body
	// at all, so there is no branch here and no way for the two paths to
	// drift apart.
	if err := c.do(ctx, http.MethodPost, "/api/v2/tunnels", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RotateToken issues a new credential for a tunnel and invalidates the old one.
//
// It is the only way to obtain a credential for a tunnel that already exists,
// and therefore the only way to make an adopted tunnel usable. The cost is
// stated plainly because it is immediate: whatever was running with the old
// credential stops working at its next poll and stays broken until it is given
// the new one. Nothing else about the tunnel changes: not the address, not the
// key pair, not the routes.
func (c *TunnelClient) RotateToken(ctx context.Context, id string) (*RotateTokenResult, error) {
	var out RotateTokenResult
	path := "/api/v2/tunnels/" + url.PathEscape(id) + "/rotate-token"
	if err := c.do(ctx, http.MethodPost, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FindTunnelByExternalID looks a tunnel up by the identifier its creator chose,
// within the account behind the token.
//
// Returns nil and no error when there is none. That is not laziness about
// errors: the API answers an empty list rather than a 404, because the question
// is about a set, and "none" is a real answer to it that a caller will hit
// routinely.
func (c *TunnelClient) FindTunnelByExternalID(ctx context.Context, externalID string) (*TunnelSummary, error) {
	var out struct {
		Data []TunnelSummary `json:"data"`
	}
	path := "/api/v2/tunnels?external_id=" + url.QueryEscape(externalID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, nil
	}
	return &out.Data[0], nil
}

// GetTunnel reads one tunnel and its routes.
//
// The answer is a different shape from the create's: the tunnel is nested under
// `tunnel`, its identifier is `id`, and neither the tunnel token nor the
// WireGuard private key is in it.
func (c *TunnelClient) GetTunnel(ctx context.Context, id string) (*TunnelWithRoutes, error) {
	var out TunnelWithRoutes
	if err := c.do(ctx, http.MethodGet, "/api/v2/tunnels/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListTunnels returns every tunnel of the account, following the pagination to
// the end.
//
// The listing is paged and its default page is small, so a caller that read the
// first page only would quietly see a prefix of the account's tunnels, which
// as a data source means Terraform planning against a subset of what exists.
//
// The loop advances by however many rows came back rather than by the page size
// asked for: a server free to cap the size is still walked correctly, and the
// end is an empty page rather than a short one.
func (c *TunnelClient) ListTunnels(ctx context.Context) ([]TunnelSummary, error) {
	var all []TunnelSummary
	offset := 0

	for page := 0; page < listPageLimit; page++ {
		var out struct {
			Data   []TunnelSummary `json:"data"`
			Limit  int             `json:"limit"`
			Offset int             `json:"offset"`
		}
		path := "/api/v2/tunnels?limit=" + strconv.Itoa(tunnelPageSize) +
			"&offset=" + strconv.Itoa(offset)
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		if len(out.Data) == 0 {
			return all, nil
		}
		all = append(all, out.Data...)
		offset += len(out.Data)
	}

	return all, nil
}

// DeleteTunnel removes a tunnel and everything hanging off it.
func (c *TunnelClient) DeleteTunnel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v2/tunnels/"+url.PathEscape(id), nil, nil)
}

// ─────────────────────────────────────────────────────────────────────────────
// Routes
// ─────────────────────────────────────────────────────────────────────────────

// CreateRoute adds a route to a tunnel. The route is enabled when created; see
// CreateRouteInput.
func (c *TunnelClient) CreateRoute(ctx context.Context, tunnelID string, in CreateRouteInput) (*Route, error) {
	var out Route
	if err := c.do(ctx, http.MethodPost, routesPath(tunnelID), in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRoute reads one route. Unlike a tunnel, a route reads back complete, so
// importing one loses nothing.
func (c *TunnelClient) GetRoute(ctx context.Context, tunnelID, routeID string) (*Route, error) {
	var out Route
	if err := c.do(ctx, http.MethodGet, routePath(tunnelID, routeID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRoutes returns the routes of a tunnel. This listing is not paged: the
// answer is the whole set.
func (c *TunnelClient) ListRoutes(ctx context.Context, tunnelID string) ([]Route, error) {
	var out struct {
		Routes []Route `json:"routes"`
		Total  int     `json:"total"`
	}
	if err := c.do(ctx, http.MethodGet, routesPath(tunnelID), nil, &out); err != nil {
		return nil, err
	}
	return out.Routes, nil
}

// UpdateRoute changes the fields that can be changed. A nil field is left
// alone, which is why the input is all pointers. See UpdateRouteInput for what
// is deliberately missing from it.
func (c *TunnelClient) UpdateRoute(ctx context.Context, tunnelID, routeID string, in UpdateRouteInput) (*Route, error) {
	var out Route
	if err := c.do(ctx, http.MethodPut, routePath(tunnelID, routeID), in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteRoute removes a route.
func (c *TunnelClient) DeleteRoute(ctx context.Context, tunnelID, routeID string) error {
	return c.do(ctx, http.MethodDelete, routePath(tunnelID, routeID), nil, nil)
}

func routesPath(tunnelID string) string {
	return "/api/v2/tunnels/" + url.PathEscape(tunnelID) + "/routes"
}

func routePath(tunnelID, routeID string) string {
	return routesPath(tunnelID) + "/" + url.PathEscape(routeID)
}
