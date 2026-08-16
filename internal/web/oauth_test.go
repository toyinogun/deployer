package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// The verifier and its challenge, from RFC 7636 appendix B, so the test drives
// PKCE against the specification's own worked example.
const (
	testVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	testChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	testRedirect  = "http://localhost/callback"
)

// registerConnector runs a real registration and hands back the client id.
func (h *harness) registerConnector(t *testing.T, name string, uris ...string) string {
	t.Helper()
	if len(uris) == 0 {
		uris = []string{testRedirect}
	}
	rec := h.postJSON(t, identity.RegisterPath, map[string]any{
		"client_name":   name,
		"redirect_uris": uris,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("registering: got %d, want 201: %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("reading the registration: %v", err)
	}
	id, _ := body["client_id"].(string)
	if id == "" {
		t.Fatalf("the registration issued no client id: %s", rec.Body)
	}
	return id
}

// postJSON posts a JSON body, which is what a machine client sends.
func (h *harness) postJSON(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// authorizeQuery is a well formed authorize request for this client.
func authorizeQuery(clientID string) url.Values {
	return url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {testRedirect},
		"response_type":         {"code"},
		"code_challenge":        {testChallenge},
		"code_challenge_method": {"S256"},
		"resource":              {testMCPURL + "/mcp"},
		"state":                 {"opaque-state"},
		"scope":                 {"deploy"},
	}
}

// approve drives the consent page and returns the authorization code the
// redirect carried.
func (h *harness) approve(t *testing.T, session *http.Cookie, clientID string) string {
	t.Helper()
	form := authorizeQuery(clientID)
	form.Set(csrfField, h.csrfFor(t, session))
	form.Set("approve", "yes")
	rec := h.postRaw(t, identity.AuthorizePath, form, nil, session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approving: got %d, want 303: %s", rec.Code, rec.Body)
	}
	return codeFrom(t, rec.Header().Get("Location"))
}

// codeFrom reads the code out of a redirect the way a client would.
func codeFrom(t *testing.T, location string) string {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parsing the redirect %q: %v", location, err)
	}
	code := u.Query().Get("code")
	if code == "" {
		t.Fatalf("the redirect carried no code: %s", location)
	}
	return code
}

// exchange posts a token request the way a client does, form encoded.
func (h *harness) exchange(t *testing.T, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, identity.TokenPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// tokenForm is a well formed exchange for a code.
func tokenForm(clientID, code string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {testRedirect},
		"client_id":     {clientID},
		"code_verifier": {testVerifier},
	}
}

// AC-3, AC-3a. What a client reads to find out how to sign in.
func TestTheAuthorizationServerDocumentSaysWhatThisPlatformSupports(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	rec := h.get(t, identity.AuthorizationServerPath, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if doc["issuer"] != testConsoleURL {
		t.Errorf("issuer is %v, want %q", doc["issuer"], testConsoleURL)
	}
	for field, want := range map[string]string{
		"authorization_endpoint": testConsoleURL + identity.AuthorizePath,
		"token_endpoint":         testConsoleURL + identity.TokenPath,
		"registration_endpoint":  testConsoleURL + identity.RegisterPath,
	} {
		if doc[field] != want {
			t.Errorf("%s is %v, want %q", field, doc[field], want)
		}
	}
	if doc["authorization_response_iss_parameter_supported"] != true {
		t.Error("the document does not advertise the iss parameter")
	}
	// AC-3a. Advertising offline_access makes Claude ask for a refresh token,
	// and this platform issues none.
	scopes, _ := doc["scopes_supported"].([]any)
	if len(scopes) != 1 || scopes[0] != identity.ConnectorScope {
		t.Errorf("scopes_supported is %v, want exactly [%q]", scopes, identity.ConnectorScope)
	}
	if _, present := doc["client_id_metadata_document_supported"]; present {
		t.Error("the document advertises metadata documents, so a client would not fall back to registration")
	}
	methods, _ := doc["token_endpoint_auth_methods_supported"].([]any)
	if len(methods) != 1 || methods[0] != "none" {
		t.Errorf("token_endpoint_auth_methods_supported is %v, want [\"none\"]", methods)
	}
}

// The whole thread, end to end: register, approve, exchange, and the token that
// comes out is an ordinary one (AC-4, AC-9, AC-16, AC-17, AC-19, AC-20).
func TestAConnectorIsAddedAndEndsUpHoldingAnOrdinaryToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")

	// The consent page names the address the platform can verify.
	page := h.get(t, identity.AuthorizePath+"?"+authorizeQuery(clientID).Encode(), session)
	if page.Code != http.StatusOK {
		t.Fatalf("the approval page: got %d, want 200: %s", page.Code, page.Body)
	}
	if !strings.Contains(page.Body.String(), "localhost") {
		t.Error("the approval page does not name the redirect host")
	}

	code := h.approve(t, session, clientID)
	rec := h.exchange(t, tokenForm(clientID, code))
	if rec.Code != http.StatusOK {
		t.Fatalf("exchanging: got %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control is %q, want no-store", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the token response: %v", err)
	}
	raw, _ := body["access_token"].(string)
	if raw == "" {
		t.Fatalf("no access token: %s", rec.Body)
	}
	if body["token_type"] != "Bearer" || body["scope"] != identity.ConnectorScope {
		t.Errorf("the response says %v / %v, want Bearer / %s", body["token_type"], body["scope"], identity.ConnectorScope)
	}
	// No expiry and no refresh token: this platform issues neither, and saying
	// otherwise would have a client trying to refresh (AC-19).
	if _, present := body["expires_in"]; present {
		t.Error("the response carries expires_in")
	}
	if _, present := body["refresh_token"]; present {
		t.Error("the response carries a refresh token")
	}

	// AC-20. It is an api_tokens row like any other: it resolves, it lists, and
	// revoking it there stops it.
	account, _, err := h.store.ResolveToken(t.Context(), auth.HashToken(raw))
	if err != nil {
		t.Fatalf("the granted token does not authenticate: %v", err)
	}
	if account.ID != h.accountID(t, session) {
		t.Errorf("the token belongs to %s, want the approving account", account.ID)
	}
	list := h.get(t, "/tokens", session)
	if !strings.Contains(list.Body.String(), "Claude Desktop") {
		t.Error("the granted token is not in the token list")
	}

	// AC-23. One audit row, the token as the target and the client in the reason.
	found := false
	for _, row := range h.audit.rows {
		if row.Action == auth.ActionConnectorGrant {
			found = true
			if row.TargetType != "api_token" || row.TargetID == "" {
				t.Errorf("the grant row targets %q/%q, want an api_token", row.TargetType, row.TargetID)
			}
			if row.Reason != clientID {
				t.Errorf("the grant row names client %q, want %q", row.Reason, clientID)
			}
			if !row.Allowed {
				t.Error("the grant row is recorded as refused")
			}
		}
	}
	if !found {
		t.Error("the exchange wrote no connector_grant row")
	}
}

// AC-7. Registration is an unauthenticated public write, so it fills no audit.
func TestRegisteringAClientWritesNoAuditRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.registerConnector(t, "a stranger's client")
	if len(h.audit.rows) != 0 {
		t.Errorf("registration wrote %d audit rows, want none", len(h.audit.rows))
	}
}

// AC-4a, AC-5. The two refusals never overlap.
func TestARegistrationIsRefusedWithTheRightCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no redirect uris", map[string]any{"client_name": "x"}, "invalid_client_metadata"},
		{"an empty list", map[string]any{"client_name": "x", "redirect_uris": []string{}}, "invalid_client_metadata"},
		{"a name past the bound", map[string]any{
			"client_name":   strings.Repeat("n", identity.MaxClientNameLen+1),
			"redirect_uris": []string{testRedirect},
		}, "invalid_client_metadata"},
		{"plain http off the loopback", map[string]any{
			"client_name": "x", "redirect_uris": []string{"http://example.org/cb"},
		}, "invalid_redirect_uri"},
		{"a javascript uri", map[string]any{
			"client_name": "x", "redirect_uris": []string{"javascript:alert(1)"},
		}, "invalid_redirect_uri"},
		{"one bad uri among good ones", map[string]any{
			"client_name": "x", "redirect_uris": []string{testRedirect, "ftp://example.org/cb"},
		}, "invalid_redirect_uri"},
		{"more than ten", map[string]any{
			"client_name":   "x",
			"redirect_uris": manyURIs(identity.MaxRedirectURIs + 1),
		}, "invalid_redirect_uri"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.postJSON(t, identity.RegisterPath, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if body["error"] != tc.want {
				t.Errorf("error is %q, want %q", body["error"], tc.want)
			}
		})
	}
}

// manyURIs builds n distinct acceptable redirect URIs.
func manyURIs(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, "https://example.org/cb"+string(rune('a'+i)))
	}
	return out
}

// AC-10, AC-10c. The order that stops this being an open redirect: neither an
// unknown client nor an unmatched redirect URI is ever answered with a redirect.
func TestAnUnmatchedClientOrRedirectRendersAPageAndNeverRedirects(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")

	cases := []struct {
		name  string
		muted func(url.Values)
	}{
		{"an unknown client", func(q url.Values) { q.Set("client_id", "oac_nosuchclient") }},
		{"an empty client", func(q url.Values) { q.Set("client_id", "") }},
		{"a redirect nobody registered", func(q url.Values) { q.Set("redirect_uri", "https://evil.example/steal") }},
		{"a registered uri with an added path", func(q url.Values) { q.Set("redirect_uri", testRedirect+"/more") }},
		{"no redirect at all", func(q url.Values) { q.Del("redirect_uri") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := authorizeQuery(clientID)
			tc.muted(q)
			rec := h.get(t, identity.AuthorizePath+"?"+q.Encode(), session)
			if rec.Code == http.StatusSeeOther || rec.Code == http.StatusFound {
				t.Fatalf("the endpoint redirected to %q", rec.Header().Get("Location"))
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("got %d, want 400", rec.Code)
			}
			// AC-10c. Nothing the caller supplied is on the page, so there is
			// nothing on it to escape.
			body := rec.Body.String()
			for _, leaked := range []string{"evil.example", "oac_nosuchclient", clientID} {
				if strings.Contains(body, leaked) {
					t.Errorf("the error page rendered %q back at the caller", leaked)
				}
			}
		})
	}
}

// AC-11, AC-12. Everything else redirects, to an address this client registered.
func TestAnInvalidParameterRedirectsToTheRegisteredAddress(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")

	cases := []struct {
		name  string
		muted func(url.Values)
		want  string
	}{
		{"a response type this server does not issue",
			func(q url.Values) { q.Set("response_type", "token") }, "unsupported_response_type"},
		{"no code challenge at all",
			func(q url.Values) { q.Del("code_challenge") }, "invalid_request"},
		{"a plain challenge method",
			func(q url.Values) { q.Set("code_challenge_method", "plain") }, "invalid_request"},
		{"no challenge method",
			func(q url.Values) { q.Del("code_challenge_method") }, "invalid_request"},
		{"no resource",
			func(q url.Values) { q.Del("resource") }, "invalid_request"},
		{"a resource served elsewhere",
			func(q url.Values) { q.Set("resource", "https://someone-else.example/mcp") }, "invalid_target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := authorizeQuery(clientID)
			tc.muted(q)
			rec := h.get(t, identity.AuthorizePath+"?"+q.Encode(), session)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("got %d, want 303: %s", rec.Code, rec.Body)
			}
			u, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				t.Fatalf("parsing the redirect: %v", err)
			}
			if u.Scheme+"://"+u.Host+u.Path != testRedirect {
				t.Fatalf("redirected to %q, want the registered address", rec.Header().Get("Location"))
			}
			if got := u.Query().Get("error"); got != tc.want {
				t.Errorf("error is %q, want %q", got, tc.want)
			}
			if got := u.Query().Get("state"); got != "opaque-state" {
				t.Errorf("state came back %q, want it carried through", got)
			}
			if got := u.Query().Get("iss"); got != testConsoleURL {
				t.Errorf("iss is %q, want %q", got, testConsoleURL)
			}
			if u.Query().Get("code") != "" {
				t.Error("a refused authorize request still issued a code")
			}
		})
	}
}

// AC-15. Deny stamps nothing and writes no code.
func TestDenyingSendsBackAccessDeniedAndIssuesNoCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")

	form := authorizeQuery(clientID)
	form.Set(csrfField, h.csrfFor(t, session))
	form.Set("approve", "no")
	rec := h.postRaw(t, identity.AuthorizePath, form, nil, session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303: %s", rec.Code, rec.Body)
	}
	u, _ := url.Parse(rec.Header().Get("Location"))
	if got := u.Query().Get("error"); got != "access_denied" {
		t.Errorf("error is %q, want access_denied", got)
	}
	if u.Query().Get("code") != "" {
		t.Error("denying issued a code")
	}
	if got := u.Query().Get("state"); got != "opaque-state" {
		t.Errorf("state came back %q", got)
	}

	client, err := h.store.OAuthClient(t.Context(), clientID)
	if err != nil {
		t.Fatalf("reading the client: %v", err)
	}
	if client.ApprovedAt != "" {
		t.Error("denying stamped the client approved")
	}
}

// AC-13. The consent page takes the session CSRF check exactly as /tokens does.
func TestApprovingWithoutACSRFTokenIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")

	form := authorizeQuery(clientID)
	form.Set("approve", "yes")
	rec := h.postRaw(t, identity.AuthorizePath, form, nil, session)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
	if _, err := h.store.OAuthCode(t.Context(), "anything"); err == nil {
		t.Error("a refused approval wrote a code")
	}
}

// AC-13a. The warning that stands between a person and a local process
// impersonating their editor.
func TestTheLoopbackWarningShowsOnlyForAWhollyLoopbackClient(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")

	const warning = "running on your own machine"

	local := h.registerConnector(t, "A local editor")
	page := h.get(t, identity.AuthorizePath+"?"+authorizeQuery(local).Encode(), session)
	if !strings.Contains(page.Body.String(), warning) {
		t.Error("a loopback client got no warning")
	}

	remote := h.registerConnector(t, "A hosted client", "https://hosted.example/callback")
	q := authorizeQuery(remote)
	q.Set("redirect_uri", "https://hosted.example/callback")
	page = h.get(t, identity.AuthorizePath+"?"+q.Encode(), session)
	if strings.Contains(page.Body.String(), warning) {
		t.Error("a hosted client got the local machine warning")
	}
}

// AC-10a, AC-18. The whole point of the loopback port relaxation is that the
// client is listening on the port it asked with, so the redirect has to carry
// that port and the exchange has to accept it back. Driving it through the
// handlers is what makes this real: the unit test on MatchRedirectURI discarded
// the matched string, so the registered port less form travelled all the way to
// the redirect and onto the code, and the flow every native client uses could
// not complete.
func TestALoopbackClientIsRedirectedToThePortItAskedWith(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "A local editor")

	const ephemeral = "http://localhost:54321/callback"
	form := authorizeQuery(clientID)
	form.Set("redirect_uri", ephemeral)
	form.Set(csrfField, h.csrfFor(t, session))
	form.Set("approve", "yes")
	rec := h.postRaw(t, identity.AuthorizePath, form, nil, session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approving: got %d, want 303: %s", rec.Code, rec.Body)
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, ephemeral+"?") {
		t.Fatalf("the code went to %q, want the requested address %q", location, ephemeral)
	}

	exchange := tokenForm(clientID, codeFrom(t, location))
	exchange.Set("redirect_uri", ephemeral)
	if got := h.exchange(t, exchange); got.Code != http.StatusOK {
		t.Fatalf("exchanging with the address the client used: got %d, want 200: %s", got.Code, got.Body)
	}
}

// AC-13, AC-20a. A hostile name is escaped on the page and sane in the list.
func TestAHostileClientNameIsEscapedAndBounded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	const hostile = `<script>alert(1)</script>Deployer Official`
	clientID := h.registerConnector(t, hostile)

	page := h.get(t, identity.AuthorizePath+"?"+authorizeQuery(clientID).Encode(), session)
	if strings.Contains(page.Body.String(), "<script>alert(1)</script>") {
		t.Error("the client's name rendered as markup on the approval page")
	}
	if !strings.Contains(page.Body.String(), "&lt;script&gt;") {
		t.Error("the client's name is not on the page at all, escaped or otherwise")
	}

	code := h.approve(t, session, clientID)
	if rec := h.exchange(t, tokenForm(clientID, code)); rec.Code != http.StatusOK {
		t.Fatalf("exchanging: got %d: %s", rec.Code, rec.Body)
	}
	list := h.get(t, "/tokens", session)
	if strings.Contains(list.Body.String(), "<script>alert(1)</script>") {
		t.Error("the token name rendered as markup in the list")
	}
}

// AC-9a. Signing in from the gate comes back to the same authorize URL, whole.
func TestTheSignInGateReturnsToTheAuthorizeURLWithItsQueryIntact(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	// An account exists, but this request arrives holding no session.
	h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")

	target := identity.AuthorizePath + "?" + authorizeQuery(clientID).Encode()
	rec := h.get(t, target, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", rec.Code)
	}
	gate, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parsing the gate redirect: %v", err)
	}
	next := gate.Query().Get("next")
	if next != target {
		t.Fatalf("next is %q, want the whole authorize url %q", next, target)
	}
	// And the round trip: signing in from there lands back on the page.
	form := url.Values{"email": {"owner@example.org"}, "password": {testPassword}, "next": {next}}
	back := h.post(t, "/login", form, nil, nil)
	if back.Code != http.StatusSeeOther {
		t.Fatalf("signing in: got %d: %s", back.Code, back.Body)
	}
	if got := back.Header().Get("Location"); got != target {
		t.Errorf("signing in landed on %q, want %q", got, target)
	}
}

// AC-14. A suspended account never reaches the approval page, and the refusal
// lands before anything is written. Suspension takes the session away first, so
// what this account meets is the gate rather than a consent page.
func TestASuspendedAccountNeverReachesTheApprovalPage(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	admin := h.signIn(t, "admin@example.org")
	victim := h.signIn(t, "victim@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")
	victimID := h.accountID(t, victim)

	form := url.Values{csrfField: {h.csrfFor(t, admin)}, "confirm_email": {"victim@example.org"}}
	if rec := h.postRaw(t, "/admin/accounts/"+victimID+"/disable", form, nil, admin); rec.Code != http.StatusSeeOther {
		t.Fatalf("suspending: got %d: %s", rec.Code, rec.Body)
	}

	rec := h.get(t, identity.AuthorizePath+"?"+authorizeQuery(clientID).Encode(), victim)
	if rec.Code == http.StatusOK {
		t.Fatal("a suspended account was shown the approval page")
	}
	if strings.Contains(rec.Body.String(), "Approve") {
		t.Error("the approval control rendered for a suspended account")
	}

	// And nothing was written on the way to that refusal.
	client, err := h.store.OAuthClient(t.Context(), clientID)
	if err != nil {
		t.Fatalf("reading the client: %v", err)
	}
	if client.ApprovedAt != "" {
		t.Error("a suspended account's visit stamped the client approved")
	}
}

// AC-18. Every way an exchange fails is the same answer, and it says nothing
// about which check refused it.
func TestAnExchangeIsRefusedIdenticallyHoweverItIsWrong(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		muted func(url.Values)
	}{
		{"a wrong verifier", func(f url.Values) { f.Set("code_verifier", "not-the-verifier-that-made-it") }},
		{"no verifier", func(f url.Values) { f.Del("code_verifier") }},
		{"a made up code", func(f url.Values) { f.Set("code", "nosuchcode") }},
		{"another client's id", func(f url.Values) { f.Set("client_id", "oac_someoneelse") }},
		{"a different redirect uri", func(f url.Values) { f.Set("redirect_uri", "http://localhost:9/callback") }},
		{"a resource the code was not bound to", func(f url.Values) { f.Set("resource", "https://elsewhere.example/mcp") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			session := h.signIn(t, "owner@example.org")
			clientID := h.registerConnector(t, "Claude Desktop")
			code := h.approve(t, session, clientID)

			form := tokenForm(clientID, code)
			tc.muted(form)
			rec := h.exchange(t, form)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if body["error"] != "invalid_grant" {
				t.Errorf("error is %q, want invalid_grant", body["error"])
			}
			// The description must not name the check that failed.
			for _, telling := range []string{"verifier", "pkce", "redirect", "client", "resource", "expired", "consumed"} {
				if strings.Contains(strings.ToLower(body["error_description"]), telling) {
					t.Errorf("the description says which check failed: %q", body["error_description"])
				}
			}
		})
	}
}

// AC-16a. A code presented twice is refused, and it costs the token the first
// presentation issued.
func TestReplayingACodeRefusesItAndRevokesTheTokenItIssued(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")
	code := h.approve(t, session, clientID)

	first := h.exchange(t, tokenForm(clientID, code))
	if first.Code != http.StatusOK {
		t.Fatalf("the first exchange: got %d: %s", first.Code, first.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	raw, _ := body["access_token"].(string)
	if _, _, err := h.store.ResolveToken(t.Context(), auth.HashToken(raw)); err != nil {
		t.Fatalf("the first token does not work: %v", err)
	}

	second := h.exchange(t, tokenForm(clientID, code))
	if second.Code != http.StatusBadRequest {
		t.Fatalf("the replay: got %d, want 400: %s", second.Code, second.Body)
	}
	if _, _, err := h.store.ResolveToken(t.Context(), auth.HashToken(raw)); err == nil {
		t.Error("the replay left the token the first exchange issued still working")
	}
}

// AC-16a. A code past its 60 seconds is worth nothing.
func TestACodePastItsMinuteIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")
	code := h.approve(t, session, clientID)

	h.clock.Advance(identity.CodeLifetime + time.Second)
	if rec := h.exchange(t, tokenForm(clientID, code)); rec.Code != http.StatusBadRequest {
		t.Errorf("an expired code exchanged: got %d: %s", rec.Code, rec.Body)
	}
}

// AC-17. Form encoded and nothing else, because a JSON only parser answers 415
// and a client reads that as an outage.
func TestTheTokenEndpointTakesFormEncodedBodiesOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	rec := h.postJSON(t, identity.TokenPath, map[string]any{"grant_type": "authorization_code"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a JSON body got %d, want 400", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body["error"] != "invalid_request" {
		t.Errorf("error is %q, want invalid_request", body["error"])
	}
}

// AC-17. And a grant type this server does not issue is told so plainly.
func TestAnUnknownGrantTypeIsRefusedAsSuch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	rec := h.exchange(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body["error"] != "unsupported_grant_type" {
		t.Errorf("error is %q, want unsupported_grant_type", body["error"])
	}
}

// AC-19. A second grant for one client replaces the first rather than adding to
// it, so a person never accumulates tokens for one connector.
func TestASecondGrantForOneClientReplacesTheFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")

	raws := make([]string, 0, 2)
	for range 2 {
		code := h.approve(t, session, clientID)
		rec := h.exchange(t, tokenForm(clientID, code))
		if rec.Code != http.StatusOK {
			t.Fatalf("exchanging: got %d: %s", rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		raw, _ := body["access_token"].(string)
		raws = append(raws, raw)
	}

	if _, _, err := h.store.ResolveToken(t.Context(), auth.HashToken(raws[0])); err == nil {
		t.Error("the first grant still works after the second replaced it")
	}
	if _, _, err := h.store.ResolveToken(t.Context(), auth.HashToken(raws[1])); err != nil {
		t.Errorf("the second grant does not work: %v", err)
	}
}

// AC-20. Revoking a granted token on /tokens stops it, exactly as it stops one
// that was minted by hand.
func TestRevokingAGrantedTokenOnTheTokenPageStopsIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")
	code := h.approve(t, session, clientID)

	rec := h.exchange(t, tokenForm(clientID, code))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	raw, _ := body["access_token"].(string)
	_, tok, err := h.store.ResolveToken(t.Context(), auth.HashToken(raw))
	if err != nil {
		t.Fatalf("the granted token does not work: %v", err)
	}

	form := url.Values{csrfField: {h.csrfFor(t, session)}}
	if rev := h.postRaw(t, "/tokens/"+tok.ID+"/revoke", form, nil, session); rev.Code != http.StatusSeeOther {
		t.Fatalf("revoking: got %d: %s", rev.Code, rev.Body)
	}
	if _, _, err := h.store.ResolveToken(t.Context(), auth.HashToken(raw)); err == nil {
		t.Error("the granted token still works after being revoked on /tokens")
	}
}

// AC-25b. Every one of the five registrations names its method, which is what
// makes a wrong verb a refusal rather than a handler running on a request it was
// not written for.
//
// Where that refusal is distinguishable from an unknown path depends on the
// hostname, and the difference is the console catch all rather than anything
// this feature added. On the bare pattern there is no catch all, so the standard
// mux answers 405 for a wrong method on a path it holds. On the console host the
// `<host>/` subtree pattern matches every verb, so it claims the request first
// and a wrong method reads as 404, exactly as it already does for /login and
// every other page. That is the safer direction and it is the behaviour of the
// whole surface, not of these routes.
func TestAWrongMethodOnAnOAuthRouteIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	wrong := []struct{ method, path string }{
		{http.MethodPost, identity.AuthorizationServerPath},
		{http.MethodGet, identity.RegisterPath},
		{http.MethodGet, identity.TokenPath},
		{http.MethodDelete, identity.AuthorizePath},
	}
	for _, tc := range wrong {
		// On the bare pattern the mux tells the two refusals apart.
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s on the bare pattern: got %d, want 405", tc.method, tc.path, rec.Code)
		}
		// On the console host the catch all answers first, and either way no
		// handler ran.
		req = httptest.NewRequest(tc.method, "http://"+testConsoleHost+tc.path, nil)
		rec = httptest.NewRecorder()
		h.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the console host: got %d, want 404", tc.method, tc.path, rec.Code)
		}
	}

	// And a path nobody registered is the console host's own 404, unchanged.
	req := httptest.NewRequest(http.MethodGet, "http://"+testConsoleHost+"/oauth/nothing", nil)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("an unregistered oauth path: got %d, want 404", rec.Code)
	}
}

// AC-6, AC-22. Adding a connector must never spend the sign in allowance, or a
// person could lock themselves out of the console they are signing in to.
func TestAddingAConnectorDoesNotSpendTheSignInAllowance(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")

	// Four connectors is twelve spends of the connector bucket, comfortably past
	// the whole sign in allowance and inside the connector one.
	for i := range 4 {
		clientID := h.registerConnector(t, "Client")
		code := h.approve(t, session, clientID)
		if rec := h.exchange(t, tokenForm(clientID, code)); rec.Code != http.StatusOK {
			t.Fatalf("connector %d: got %d: %s", i, rec.Code, rec.Body)
		}
	}

	// The console is still reachable from this address.
	if rec := h.post(t, "/login", url.Values{
		"email": {"owner@example.org"}, "password": {testPassword},
	}, nil, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("signing in after adding connectors: got %d, want 303: %s", rec.Code, rec.Body)
	}
}

// A granted token carries its client, and a hand minted one does not. That is
// what tells the two apart in the list and in the live client index.
func TestOnlyAGrantedTokenCarriesAClient(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	clientID := h.registerConnector(t, "Claude Desktop")
	code := h.approve(t, session, clientID)
	if rec := h.exchange(t, tokenForm(clientID, code)); rec.Code != http.StatusOK {
		t.Fatalf("exchanging: got %d", rec.Code)
	}

	form := url.Values{csrfField: {h.csrfFor(t, session)}, "name": {"by hand"}}
	if rec := h.postRaw(t, "/tokens", form, nil, session); rec.Code != http.StatusOK {
		t.Fatalf("minting by hand: got %d: %s", rec.Code, rec.Body)
	}

	var withClient, withoutClient int
	rows, err := h.store.DB().QueryContext(t.Context(),
		`SELECT oauth_client_id FROM api_tokens WHERE revoked_at IS NULL`)
	if err != nil {
		t.Fatalf("reading tokens: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("closing the rows: %v", err)
		}
	}()
	for rows.Next() {
		var client *string
		if err := rows.Scan(&client); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		if client == nil {
			withoutClient++
			continue
		}
		withClient++
		if *client != clientID {
			t.Errorf("a token carries client %q, want %q", *client, clientID)
		}
	}
	if withClient != 1 || withoutClient != 1 {
		t.Errorf("%d granted and %d hand minted live tokens, want one of each", withClient, withoutClient)
	}
}

// AC-26, AC-27. The fifth tab shows the address and nothing else, and it is the
// one panel with no mint control: this client is issued its own credential
// through the approval page, so a token minted here would be a second one
// nobody needs.
func TestTheConnectorTabShowsTheAddressAndOffersNoToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	session := h.signIn(t, "owner@example.org")
	body := h.get(t, "/connect", session).Body.String()

	panel := panelFor(t, body, "claude-app")
	if !strings.Contains(panel, testMCPURL+"/mcp") {
		t.Error("the connector tab does not show the deploy address")
	}
	// Nothing else: no credential, and not even the placeholder the other four
	// carry before a mint.
	if strings.Contains(panel, tokenPlaceholder) {
		t.Error("the connector tab carries the token placeholder")
	}
	if strings.Contains(panel, "Bearer") || strings.Contains(panel, identity.APITokenPrefix) {
		t.Error("the connector tab carries a credential")
	}
	if strings.Contains(panel, "Mint a token") {
		t.Error("the connector tab offers a mint control")
	}

	// The other four still do, so this removed nothing from them.
	if !strings.Contains(panelFor(t, body, "claude-code"), "Mint a token") {
		t.Error("the Claude Code tab lost its mint control")
	}

	// AC-27. The noscript region stacks every panel, this one included, so the
	// tab is reachable with the script off.
	if !strings.Contains(body, "<noscript>") || !strings.Contains(body, ".connect-panel[hidden]") {
		t.Error("the noscript region is gone, so the fifth tab is unreachable without the script")
	}
}

// panelFor cuts one tab's panel out of the rendered page, so an assertion about
// one block cannot be satisfied by another block's text.
func panelFor(t *testing.T, body, key string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `id="panel-`+key+`"`)
	if !ok {
		t.Fatalf("no panel for %q on the page", key)
	}
	panel, _, ok := strings.Cut(rest, "</section>")
	if !ok {
		t.Fatalf("the panel for %q never ends", key)
	}
	return panel
}
