package nubulus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The error codes the operator reacts to by name. The API answers a single
// envelope, {"error": "<CODE>", "message": "<prose>"}, on every route.
//
// They are matched by NAME and never by status, for two reasons. The first is
// history: some of these used to come back with a 5xx even though every one of
// them is the caller's mistake or the caller's state, and the code in the body
// has always been the part that was right. The second outlives that fix: two
// unrelated failures share a status. A 409 is a hostname claimed by somebody
// else and a tunnel that is not active, and the advice for them is opposite.
const (
	// CodeNoAccountRole is a token with no role claim. It reads like a
	// permission problem and is not one: see Classify.
	CodeNoAccountRole = "NO_ACCOUNT_ROLE"
	// CodeNoAccount is a token whose organization maps to no account.
	CodeNoAccount = "NO_ACCOUNT"
	// CodeNotFound is the ordinary missing resource.
	CodeNotFound = "NOT_FOUND"

	// CodeInvalidInput is a malformed request: a port out of range, a hostname
	// that is not an FQDN, a path prefix that does not start with a slash.
	// Retrying it unchanged never helps.
	CodeInvalidInput = "INVALID_INPUT"
	// CodeHostnameConflict is a route hostname already in use by another
	// account. Hostnames are unique across the whole platform, so this is not
	// something the account that hits it can inspect or resolve on its own.
	CodeHostnameConflict = "HOSTNAME_CONFLICT"
	// CodeQuotaExceeded is the limit on how many tunnels an account may have.
	// Unlike the others it lifts on its own once something is deleted.
	CodeQuotaExceeded = "QUOTA_EXCEEDED"
	// CodeTunnelInactive is any operation on a tunnel that is not active.
	// Routes exist only inside an active tunnel, and retrying will not change
	// that until the tunnel itself does.
	CodeTunnelInactive = "TUNNEL_INACTIVE"
)

// APIError is a 4xx or 5xx with the service's own code and message.
type APIError struct {
	Status  int
	Code    string
	Message string

	Method string
	URL    string
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "":
		return fmt.Sprintf("%s %s: HTTP %d %s: %s", e.Method, e.URL, e.Status, e.Code, e.Message)
	case e.Message != "":
		// No code means the body was not the envelope: something in front of
		// the API answered. Whatever it said is the only clue there is, so it
		// is kept rather than reduced to a status.
		return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, e.Message)
	default:
		return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.URL, e.Status)
	}
}

// TransportError is a request that never got an answer: DNS, TLS, timeout, or
// a refused connection. It is kept apart from APIError because the advice is
// completely different: an endpoint that never answers is usually the wrong
// endpoint, or a network that does not let the request out.
type TransportError struct {
	Method string
	URL    string
	Err    error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("%s %s: %v", e.Method, e.URL, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// parseAPIError turns a failed response into an APIError, tolerating a body
// that is not the envelope: anything answering on behalf of the API (a proxy,
// a load balancer, an error page) replies in plain text, and losing that text
// would leave the user with a bare status code.
func parseAPIError(method, url string, resp *http.Response) error {
	apiErr := &APIError{Status: resp.StatusCode, Method: method, URL: url}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || len(raw) == 0 {
		return apiErr
	}

	var envelope struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error != "" {
		apiErr.Code = envelope.Error
		apiErr.Message = envelope.Message
		return apiErr
	}

	apiErr.Message = strings.TrimSpace(string(raw))
	return apiErr
}

// StatusOf returns the HTTP status of err, or 0 when it is not an APIError.
func StatusOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

// CodeOf returns the service's error code, or "".
func CodeOf(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// IsNotFound reports whether err is a 404. It is the one an operator hits
// routinely and legitimately: something it created was deleted behind its back.
func IsNotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

// Failure is what a controller needs to know about an error, which is less than
// a human needs and a different shape.
type Failure struct {
	// Permanent says retrying the identical request cannot succeed until
	// something outside the operator changes: a field in the object, a
	// credential, another account releasing a hostname.
	//
	// It decides the requeue policy, and getting it backwards is expensive in
	// both directions. Treating a permanent failure as transient turns one bad
	// object into a loop that hammers the API for as long as it exists, which
	// is the difference between a controller and a command anybody runs by
	// hand. Treating a transient one as permanent leaves an object stuck until
	// somebody touches it.
	Permanent bool

	// Reason is a CamelCase condition reason, as Kubernetes conditions want.
	Reason string

	// Message is one sentence for the condition, written for whoever is running
	// `kubectl describe` and has no idea what any of this is called internally.
	Message string
}

// Classify turns an error into the requeue decision and the words that go on
// the object.
//
// The three failures people hit most are all properties of the CREDENTIAL, and
// none of them says so. The honest message from the service sends you to look
// in the wrong place for every one. That is most of what this function is for.
//
// The cases that match on a CODE come first, and must keep coming first: a code
// arriving with an unexpected status would otherwise be explained as whatever
// that status usually means.
func Classify(err error) Failure {
	var transportErr *TransportError
	if errors.As(err, &transportErr) {
		return Failure{
			Permanent: false,
			Reason:    "EndpointUnreachable",
			Message: "The API did not answer at all. Check that it is reachable from this cluster: " +
				transportErr.Error(),
		}
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return Failure{Permanent: false, Reason: "Error", Message: err.Error()}
	}

	switch {
	case apiErr.Code == CodeNoAccountRole:
		return Failure{
			Permanent: true,
			Reason:    "CredentialMissingRoleScope",
			Message: "The credential was accepted but carries no role claim, so the API refuses every " +
				"request made with it. This is the token and not the account's permissions: an " +
				"application token only carries roles when it was requested with the scope " +
				"urn:zitadel:iam:org:projects:roles (\"projects\", plural). The operator asks for it, " +
				"so a failure here means the client_id and client_secret are not an application token.",
		}

	case apiErr.Code == CodeNoAccount:
		return Failure{
			Permanent: true,
			Reason:    "CredentialNotMappedToAccount",
			Message: "The credential is valid but the organization it was issued in maps to no account. " +
				"This usually means it belongs to a different environment than the endpoint it is " +
				"being used against.",
		}

	case apiErr.Code == CodeInvalidInput:
		return Failure{
			Permanent: true,
			Reason:    "InvalidSpec",
			Message:   "The API refused the request as malformed, so this is the spec: " + apiErr.Message,
		}

	case apiErr.Code == CodeHostnameConflict:
		return Failure{
			Permanent: true,
			Reason:    "HostnameConflict",
			Message: "A hostname may only be routed by one account, and this one is held elsewhere. " +
				"Nothing in this account is holding it, so it cannot be found or freed from here.",
		}

	case apiErr.Code == CodeQuotaExceeded:
		// The only refusal that lifts without anybody editing anything, so it
		// is the only 4xx worth retrying on its own.
		return Failure{
			Permanent: false,
			Reason:    "QuotaExceeded",
			Message:   "The account has reached the number of tunnels it may have.",
		}

	case apiErr.Code == CodeTunnelInactive:
		// Also self-clearing: a tunnel becomes active on its own.
		return Failure{
			Permanent: false,
			Reason:    "TunnelNotActive",
			Message:   "Routes live inside an active tunnel, and this one is not active yet.",
		}

	case apiErr.Status == http.StatusUnauthorized, apiErr.Status == http.StatusForbidden:
		if apiErr.Code == "" {
			// Nothing that comes from the API is missing the envelope, so a bare
			// 4xx was answered by something in front of it. Saying "check your
			// credential" here would be a confident lie.
			return Failure{
				Permanent: false,
				Reason:    "RefusedBeforeReachingAPI",
				Message: fmt.Sprintf("HTTP %d with no error code, which means the request was refused "+
					"before it reached the API. Check what this cluster's traffic goes through on the "+
					"way out.", apiErr.Status),
			}
		}
		return Failure{Permanent: true, Reason: "Forbidden", Message: apiErr.Error()}

	case apiErr.Status == http.StatusTooManyRequests:
		return Failure{
			Permanent: false,
			Reason:    "RateLimited",
			Message:   "The API is rate limiting this account. Backing off.",
		}

	case apiErr.Status >= 500:
		return Failure{Permanent: false, Reason: "APIError", Message: apiErr.Error()}
	}

	return Failure{Permanent: true, Reason: "Rejected", Message: apiErr.Error()}
}
