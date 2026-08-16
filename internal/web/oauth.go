package web

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// The OAuth error codes these three routes answer. They are RFC 6749 codes
// rather than internal/domain reason codes, which spec 0024 records as a written
// exception covering exactly these routes and no others (AC-24).
const (
	errInvalidRequest       = "invalid_request"
	errInvalidClientData    = "invalid_client_metadata"
	errInvalidRedirectURI   = "invalid_redirect_uri"
	errUnsupportedResponse  = "unsupported_response_type"
	errUnsupportedGrantType = "unsupported_grant_type"
	errInvalidGrant         = "invalid_grant"
	errInvalidTarget        = "invalid_target"
	errAccessDenied         = "access_denied"
)

// registerBodyLimit bounds a registration body. Every field in one is a short
// string and a bounded list, so anything larger is not a real client.
const registerBodyLimit = 8 << 10

// authorizationServer is the RFC 8414 document. It carries no
// client_id_metadata_document_supported, so a client falls back to registration,
// and its scopes deliberately exclude offline_access: Claude appends that when
// it is advertised in order to obtain a refresh token, and this platform issues
// none (spec 0024, AC-3, AC-3a).
type authorizationServer struct {
	Issuer                 string   `json:"issuer"`
	AuthorizationEndpoint  string   `json:"authorization_endpoint"`
	TokenEndpoint          string   `json:"token_endpoint"`
	RegistrationEndpoint   string   `json:"registration_endpoint"`
	ResponseTypesSupported []string `json:"response_types_supported"`
	GrantTypesSupported    []string `json:"grant_types_supported"`
	ChallengeMethods       []string `json:"code_challenge_methods_supported"`
	TokenAuthMethods       []string `json:"token_endpoint_auth_methods_supported"`
	ScopesSupported        []string `json:"scopes_supported"`
	IssuerParamSupported   bool     `json:"authorization_response_iss_parameter_supported"`
}

// authServerDocument answers the authorization server metadata. No credential:
// this is how a client that holds none finds out what to do.
func (s *Server) authServerDocument(w http.ResponseWriter, r *http.Request) {
	base := s.opts.ConsoleURL
	s.writeOAuthJSON(w, r, http.StatusOK, authorizationServer{
		Issuer:                 base,
		AuthorizationEndpoint:  base + identity.AuthorizePath,
		TokenEndpoint:          base + identity.TokenPath,
		RegistrationEndpoint:   base + identity.RegisterPath,
		ResponseTypesSupported: []string{"code"},
		GrantTypesSupported:    []string{"authorization_code"},
		ChallengeMethods:       []string{"S256"},
		TokenAuthMethods:       []string{"none"},
		ScopesSupported:        []string{identity.ConnectorScope},
		IssuerParamSupported:   true,
	})
}

// registrationRequest is the RFC 7591 body. Every other field a client sends is
// ignored rather than refused, because a client is free to describe itself and
// this platform stores only what it uses.
type registrationRequest struct {
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// registrationResponse echoes back what was accepted. No client secret is ever
// issued: every client here is public and PKCE is what binds the exchange
// (spec 0024, AC-4, AC-21).
type registrationResponse struct {
	ClientID              string   `json:"client_id"`
	ClientIDIssuedAt      int64    `json:"client_id_issued_at"`
	ClientName            string   `json:"client_name"`
	RedirectURIs          []string `json:"redirect_uris"`
	TokenEndpointAuthMeth string   `json:"token_endpoint_auth_method"`
}

// registerClient stores a client anyone on the internet may create.
//
// It writes no audit row (AC-7). This is an unauthenticated public write and the
// audit table is not somewhere strangers get to fill. What bounds it instead is
// the connector rate limit and the daily sweep of anything nobody approved.
func (s *Server) registerClient(w http.ResponseWriter, r *http.Request) {
	if !s.spendConnector(w, r) {
		return
	}
	var body registrationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, registerBodyLimit))
	if err := dec.Decode(&body); err != nil {
		s.oauthError(w, r, http.StatusBadRequest, errInvalidClientData, "the registration body could not be read")
		return
	}

	client, err := s.svc.RegisterClient(r.Context(), body.ClientName, body.RedirectURIs)
	switch {
	case errors.Is(err, identity.ErrRedirectURIInvalid):
		s.oauthError(w, r, http.StatusBadRequest, errInvalidRedirectURI, "a redirect uri was not one this platform will accept")
		return
	case errors.Is(err, identity.ErrClientMetadataInvalid):
		s.oauthError(w, r, http.StatusBadRequest, errInvalidClientData, "the registration is missing something it needs")
		return
	case err != nil:
		s.internalError(w, r, err, "registering a client")
		return
	}

	s.writeOAuthJSON(w, r, http.StatusCreated, registrationResponse{
		ClientID:              client.ID,
		ClientIDIssuedAt:      s.svc.Now().Unix(),
		ClientName:            client.Name,
		RedirectURIs:          client.RedirectURIs,
		TokenEndpointAuthMeth: "none",
	})
}

// authorizeRequest is one authorize call after the parameters have been read off
// it. Redirect is the **matched registered** URI rather than the one the caller
// sent, so nothing downstream can redirect anywhere that was not registered.
type authorizeRequest struct {
	client    identity.OAuthClient
	Redirect  string
	State     string
	Challenge string
	Resource  string
	Scope     string
}

// authorizePage is the machine half of the authorize endpoint plus the page the
// person answers on.
//
// The order here is the whole open redirect defence and it is load bearing
// (AC-10). An unknown client, or a redirect URI that matches none registered for
// it, is answered on a page here and never by a redirect, because there is no
// address the platform has agreed to send anything to. Only once a registered
// URI has been matched may any other refusal travel as a redirect.
func (s *Server) authorizePage(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.authorizeSession(w, r)
	if !ok {
		return
	}
	if !s.spendConnectorPage(w, r) {
		return
	}
	req, ok := s.resolveAuthorize(w, r, r.URL.Query())
	if !ok {
		return
	}
	s.renderApprove(w, r, account, sess, http.StatusOK, req)
}

// authorizeSubmit is the person's answer. Every parameter is resolved again from
// the form rather than trusted from the page that rendered it, because a form is
// something a caller composes.
func (s *Server) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.authorizeSession(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.refuseAuthorize(w, r)
		return
	}
	if !s.checkCSRF(w, r, account, sess) {
		return
	}
	if !s.spendConnectorPage(w, r) {
		return
	}
	req, ok := s.resolveAuthorize(w, r, r.PostForm)
	if !ok {
		return
	}

	// Deny stamps nothing and writes no code (AC-15).
	if r.PostFormValue("approve") != "yes" {
		s.redirectError(w, r, req.Redirect, req.State, errAccessDenied, "the request was denied")
		return
	}

	code, err := s.svc.ApproveClient(r.Context(), req.client.ID, account.ID, req.Redirect, req.Challenge, req.Resource)
	if err != nil {
		s.internalError(w, r, err, "approving a connector")
		return
	}
	s.redirectTo(w, r, req.Redirect, url.Values{"code": {code}}, req.State)
}

// resolveAuthorize reads and checks every authorize parameter, in the one order
// that is safe. It answers the caller itself on every failure and reports
// whether the request may go on.
func (s *Server) resolveAuthorize(w http.ResponseWriter, r *http.Request, values url.Values) (authorizeRequest, bool) {
	// First the client, then the redirect URI. Neither may be answered with a
	// redirect, because until both hold there is no address to redirect to.
	client, err := s.svc.Client(r.Context(), values.Get("client_id"))
	if errors.Is(err, identity.ErrNotFound) {
		s.refuseAuthorize(w, r)
		return authorizeRequest{}, false
	}
	if err != nil {
		s.internalError(w, r, err, "reading a connector client")
		return authorizeRequest{}, false
	}
	matched, ok := identity.MatchRedirectURI(client.RedirectURIs, values.Get("redirect_uri"))
	if !ok {
		s.refuseAuthorize(w, r)
		return authorizeRequest{}, false
	}

	// From here a refusal is a redirect to an address this client registered.
	req := authorizeRequest{
		client:    client,
		Redirect:  matched,
		State:     values.Get("state"),
		Challenge: values.Get("code_challenge"),
		Resource:  values.Get("resource"),
		Scope:     values.Get("scope"),
	}
	if values.Get("response_type") != "code" {
		s.redirectError(w, r, req.Redirect, req.State, errUnsupportedResponse, "this authorization server issues codes only")
		return authorizeRequest{}, false
	}
	// There is no path through this endpoint without PKCE (AC-11).
	if req.Challenge == "" || values.Get("code_challenge_method") != "S256" {
		s.redirectError(w, r, req.Redirect, req.State, errInvalidRequest, "a S256 code challenge is required")
		return authorizeRequest{}, false
	}
	if req.Resource == "" {
		s.redirectError(w, r, req.Redirect, req.State, errInvalidRequest, "a resource is required")
		return authorizeRequest{}, false
	}
	if !s.knownResource(req.Resource) {
		s.redirectError(w, r, req.Redirect, req.State, errInvalidTarget, "that resource is not served here")
		return authorizeRequest{}, false
	}
	return req, true
}

// knownResource is the closed set of addresses a token may be bound to. The
// value is stored on the code and compared again at the token endpoint, so a
// token is bound to the resource it was asked for (AC-12).
func (s *Server) knownResource(resource string) bool {
	return resource == s.opts.MCPURL || resource == s.opts.MCPURL+"/mcp"
}

// approveData is the approval page. Host is the only fact on it the platform can
// verify, which is why it is the heading; ClientName is what the client said
// about itself and is labelled as such (AC-13).
type approveData struct {
	Host          string
	ClientName    string
	Email         string
	Loopback      bool
	ClientID      string
	RedirectURI   string
	State         string
	Challenge     string
	Resource      string
	Scope         string
	ResponseType  string
	ChallengeMeth string
}

// renderApprove draws the consent page.
func (s *Server) renderApprove(w http.ResponseWriter, r *http.Request, account auth.Account, sess auth.Session,
	status int, req authorizeRequest,
) {
	host := req.Redirect
	if u, err := url.Parse(req.Redirect); err == nil && u.Host != "" {
		host = u.Host
	}
	s.render(w, r, account, sess, status, "authorize", "", approveData{
		Host:       host,
		ClientName: req.client.Name,
		Email:      account.Email,
		// The warning shows only when every registered address is on the
		// person's own machine, which is the case the platform cannot attribute
		// to any particular program (AC-13a).
		Loopback:      identity.AllLoopback(req.client.RedirectURIs),
		ClientID:      req.client.ID,
		RedirectURI:   req.Redirect,
		State:         req.State,
		Challenge:     req.Challenge,
		Resource:      req.Resource,
		Scope:         req.Scope,
		ResponseType:  "code",
		ChallengeMeth: "S256",
	})
}

// refuseAuthorize is the page an unmatched client or redirect URI gets. It
// renders nothing the caller supplied: not the client id, not the requested
// redirect URI, not the client name. There is therefore nothing on it to escape,
// which is the point (AC-10c).
func (s *Server) refuseAuthorize(w http.ResponseWriter, r *http.Request) {
	s.renderPublic(w, r, http.StatusBadRequest, "message", messagePage{
		Title:       "This request could not be matched",
		Message:     "Deployer could not match this connection request to a registered client. Nothing has been approved. If you were adding a connector, remove it and add it again.",
		Action:      "/connect",
		ActionLabel: "Back to the console",
	})
}

// redirectError sends an OAuth failure back to an address the client registered.
func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, redirect, state, code, description string) {
	s.redirectTo(w, r, redirect, url.Values{"error": {code}, "error_description": {description}}, state)
}

// redirectTo composes the one redirect this endpoint ever performs. iss travels
// on every one of them, so a client can tell which authorization server answered
// (AC-15, AC-16).
func (s *Server) redirectTo(w http.ResponseWriter, r *http.Request, redirect string, params url.Values, state string) {
	u, err := url.Parse(redirect)
	if err != nil {
		// Unreachable: redirect is a registered URI that already parsed.
		s.internalError(w, r, err, "composing a redirect")
		return
	}
	q := u.Query()
	for k, vs := range params {
		q.Set(k, vs[0])
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", s.opts.ConsoleURL)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// tokenExchange is the token endpoint.
//
// It authenticates no client. client_id travels in the body and PKCE binds the
// exchange, which is exactly what token_endpoint_auth_methods_supported: ["none"]
// promises (AC-21).
func (s *Server) tokenExchange(w http.ResponseWriter, r *http.Request) {
	if !s.spendConnector(w, r) {
		return
	}
	// Form encoded and nothing else. A JSON only parser would answer 415 here,
	// which reads to a client as an outage rather than as a bad request (AC-17).
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		s.oauthError(w, r, http.StatusBadRequest, errInvalidRequest, "this endpoint takes a form encoded body")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.oauthError(w, r, http.StatusBadRequest, errInvalidRequest, "the body could not be read")
		return
	}
	if r.PostFormValue("grant_type") != "authorization_code" {
		s.oauthError(w, r, http.StatusBadRequest, errUnsupportedGrantType, "this authorization server takes authorization codes only")
		return
	}

	minted, err := s.svc.Grant(r.Context(), identity.GrantRequest{
		ClientID:    r.PostFormValue("client_id"),
		Code:        r.PostFormValue("code"),
		RedirectURI: r.PostFormValue("redirect_uri"),
		Verifier:    r.PostFormValue("code_verifier"),
		Resource:    r.PostFormValue("resource"),
	})
	if errors.Is(err, identity.ErrGrantInvalid) {
		// One answer for every way it can fail, so the response never says
		// which check refused it (AC-18).
		s.oauthError(w, r, http.StatusBadRequest, errInvalidGrant, "that grant could not be exchanged")
		return
	}
	if err != nil {
		s.internalError(w, r, err, "exchanging an authorization code")
		return
	}

	// The token is the target and the client id travels in the reason, because
	// Audit holds one target pair (AC-23).
	auth.Record(r.Context(), s.auditor, auth.Audit{
		AccountID: minted.Token.AccountID, Action: auth.ActionConnectorGrant, Allowed: true,
		TargetType: "api_token", TargetID: minted.Token.ID,
		Reason: r.PostFormValue("client_id"), ClientAddress: s.clientAddress(r),
	})

	w.Header().Set("Cache-Control", "no-store")
	s.writeOAuthJSON(w, r, http.StatusOK, map[string]string{
		"access_token": minted.Raw,
		"token_type":   "Bearer",
		"scope":        identity.ConnectorScope,
	})
}

// spendConnector takes one token from the connector bucket. It is deliberately
// not the sign in bucket: adding one connector spends this three times in a row
// from one address, and sharing the sign in one would let a person adding a
// connector lock themselves out of the console they are signing in to
// (AC-6, AC-22).
func (s *Server) spendConnector(w http.ResponseWriter, r *http.Request) bool {
	if s.connectors == nil || s.connectors.Allow(s.clientAddress(r)) {
		return true
	}
	s.oauthError(w, r, http.StatusTooManyRequests, "slow_down", "wait a moment and try that again")
	return false
}

// spendConnectorPage is the same bucket for the browser half of the flow. It is
// the same spend as the machine endpoints, because adding one connector is one
// person's one action across all three, and it answers a page rather than JSON
// because a person is reading it (AC-6).
func (s *Server) spendConnectorPage(w http.ResponseWriter, r *http.Request) bool {
	if s.connectors == nil || s.connectors.Allow(s.clientAddress(r)) {
		return true
	}
	s.renderPublic(w, r, http.StatusTooManyRequests, "message", messagePage{
		Title:   "Too many attempts",
		Message: "Wait a moment and try connecting again.",
	})
	return false
}

// oauthError writes one RFC 6749 error body.
func (s *Server) oauthError(w http.ResponseWriter, r *http.Request, status int, code, description string) {
	s.writeOAuthJSON(w, r, status, map[string]string{"error": code, "error_description": description})
}

// writeOAuthJSON is the one JSON writer these routes use.
func (s *Server) writeOAuthJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.WarnContext(r.Context(), "writing an oauth response failed", "error", err)
	}
}
