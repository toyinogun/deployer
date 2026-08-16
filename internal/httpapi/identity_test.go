package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/httpapi"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
	"github.com/toyinogun/deployer/internal/suspend"
)

const goodPassword = "a long enough password"

// sentMail is a mailer that keeps what it was handed, so a test can read a link
// out of a message the way a person would read it out of their inbox.
type sentMail struct {
	mu       sync.Mutex
	messages []message
	fail     bool
}

type message struct{ To, Subject, Body string }

func (m *sentMail) Send(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, message{to, subject, body})
	if m.fail {
		return errSendFailed
	}
	return nil
}

func (m *sentMail) last(t *testing.T) message {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.messages) == 0 {
		t.Fatal("no message was sent")
	}
	return m.messages[len(m.messages)-1]
}

func (m *sentMail) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.messages)
}

type sendError struct{}

func (sendError) Error() string { return "the provider is down" }

var errSendFailed = sendError{}

// idHarness is the identity surface over a real database and a mailer a test can
// read. Nothing is mocked but the outside world.
type idHarness struct {
	mux   *http.ServeMux
	store *store.Store
	mail  *sentMail
	clock *ids.FixedClock
}

// newIDHarness builds the identity surface. withMailer false is the AC-26 state:
// no sender configured at all.
func newIDHarness(t *testing.T, withMailer bool) *idHarness {
	t.Helper()
	clock := &ids.FixedClock{T: time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)}
	st, err := store.Open(store.Options{
		Path:  filepath.Join(t.TempDir(), "deployer.db"),
		Clock: clock,
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})
	if err := st.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	as := store.ForAuth(st)
	if err := auth.Bootstrap(t.Context(), as, goodToken); err != nil {
		t.Fatalf("seeding the bootstrap account: %v", err)
	}
	authenticator := auth.NewAuthenticator(as, as).WithSessions(as, identity.SessionLifetime)

	box := &sentMail{}
	var mailer identity.Mailer
	if withMailer {
		mailer = box
	}
	svc := identity.NewService(store.ForIdentity(st), mailer, clock, identity.Options{
		PublicURL: "https://deploy.example.org",
		// A cheap hasher. This suite signs in dozens of times, and paying the
		// production cost each time buys nothing here while starving whatever
		// else `go test ./...` is running beside it. The real parameters are
		// pinned in internal/identity's own tests.
		Hasher: identity.NewHasherWith(2, 64, 1),
	})

	mux := http.NewServeMux()
	httpapi.NewIdentity(svc, authenticator, as, suspend.New(store.ForSuspend(st), svc, nil, as),
		"https://console.apps.example.org", []string{"console.apps.example.org"}, withMailer).Register(mux)
	return &idHarness{mux: mux, store: st, mail: box, clock: clock}
}

// do sends one request, optionally carrying a session cookie.
func (h *idHarness) do(t *testing.T, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader([]byte("{}"))
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// invite mints one live invite straight through the store and returns the raw
// code, which is how a test gets through the front door without an admin to
// issue one. It is the same pair of writes the boot time bootstrap makes on an
// empty database (spec 0015, AC-13).
func (h *idHarness) invite(t *testing.T) string {
	t.Helper()
	raw, err := identity.NewSecret()
	if err != nil {
		t.Fatalf("drawing an invite code: %v", err)
	}
	if _, err := h.store.CreateInvite(t.Context(), store.NewInvite{
		CodeHash:  identity.HashSecret(raw),
		ExpiresAt: ids.Stamp(h.clock.Now().Add(identity.InviteLifetime)),
	}); err != nil {
		t.Fatalf("minting an invite: %v", err)
	}
	return raw
}

// registration is one register body carrying a fresh invite, which every
// successful registration now needs (spec 0015, AC-1).
func (h *idHarness) registration(t *testing.T, email, password string) map[string]string {
	t.Helper()
	return map[string]string{"invite": h.invite(t), "email": email, "password": password}
}

// registerAndVerify walks a person from nothing to a live session, which is the
// thin thread this whole slice was built around.
func (h *idHarness) registerAndVerify(t *testing.T, email string) *http.Cookie {
	t.Helper()
	if got := h.do(t, "POST", "/v1/auth/register",
		h.registration(t, email, goodPassword), nil); got.Code != http.StatusAccepted {
		t.Fatalf("registering %s: got %d, want 202: %s", email, got.Code, got.Body)
	}
	token := linkToken(t, h.mail.last(t).Body)

	if got := h.do(t, "GET", "/v1/auth/verify?token="+token, nil, nil); got.Code != http.StatusOK {
		t.Fatalf("verifying: got %d, want 200: %s", got.Code, got.Body)
	}
	return h.signIn(t, email, goodPassword)
}

// signIn signs in and returns the session cookie.
func (h *idHarness) signIn(t *testing.T, email, password string) *http.Cookie {
	t.Helper()
	rec := h.do(t, "POST", "/v1/auth/login",
		map[string]string{"email": email, "password": password}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("signing in: got %d, want 200: %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if auth.IsSessionCookie(c.Name) {
			return c
		}
	}
	t.Fatal("no session cookie was set")
	return nil
}

// linkToken pulls the raw token out of a mailed link, the way a person's browser
// would when they click it.
func linkToken(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "?token=")
	if !ok {
		t.Fatalf("no link in the message: %q", body)
	}
	token, _, _ := strings.Cut(rest, "\n")
	return strings.TrimSpace(token)
}

// codeOf reads the error code out of a refusal body.
func codeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("reading the error body %q: %v", rec.Body, err)
	}
	return body.Error.Code
}

// TestTheThinThread is the tracer bullet: register, verify, sign in, mint. It
// covers AC-1, AC-5, AC-7 and AC-12 in one pass.
func TestTheThinThread(t *testing.T) {
	h := newIDHarness(t, true)
	cookie := h.registerAndVerify(t, "a@example.com")

	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || !cookie.Secure {
		t.Errorf("cookie flags are wrong: HttpOnly=%v SameSite=%v Secure=%v",
			cookie.HttpOnly, cookie.SameSite, cookie.Secure)
	}

	rec := h.do(t, "POST", "/v1/tokens", map[string]any{"name": "agent"}, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("minting: got %d, want 201: %s", rec.Code, rec.Body)
	}
	var minted struct {
		ID, Name, Prefix, Token string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("reading the mint body: %v", err)
	}
	if !strings.HasPrefix(minted.Token, identity.APITokenPrefix) {
		t.Errorf("token %q is not marked", minted.Token)
	}
	if minted.Prefix != minted.Token[:8] {
		t.Errorf("prefix %q does not match the token", minted.Prefix)
	}

	// The token actually authenticates on the machine route, which is the whole
	// point of minting one.
	as := store.ForAuth(h.store)
	who, err := auth.NewAuthenticator(as, as).Authenticate(t.Context(), minted.Token, "")
	if err != nil {
		t.Fatalf("the minted token did not authenticate: %v", err)
	}
	if who.Email != "a@example.com" {
		t.Errorf("the token resolved to %q, want a@example.com", who.Email)
	}
}

// TestCookieIsNotSecureOnPlainHTTP pins the derivation: the flag comes from the
// public address's scheme, not from a separate setting that could disagree.
func TestCookieIsNotSecureOnPlainHTTP(t *testing.T) {
	h := newIDHarness(t, true)
	// Rebuild the surface on an http address, everything else the same.
	as := store.ForAuth(h.store)
	authenticator := auth.NewAuthenticator(as, as).WithSessions(as, identity.SessionLifetime)
	svc := identity.NewService(store.ForIdentity(h.store), h.mail, h.clock,
		identity.Options{PublicURL: "http://localhost:8080", Hasher: identity.NewHasherWith(2, 64, 1)})
	mux := http.NewServeMux()
	httpapi.NewIdentity(svc, authenticator, as, suspend.New(store.ForSuspend(h.store), svc, nil, as),
		"http://localhost:8080", []string{"console.apps.example.org"}, true).Register(mux)
	h.mux = mux

	cookie := h.registerAndVerify(t, "a@example.com")
	if cookie.Secure {
		t.Error("a cookie handed out over plain http was marked Secure")
	}
}

// TestRegisteringATakenAddressIsIndistinguishable is AC-2: byte identical
// answers, and a different message to the address's real owner.
func TestRegisteringATakenAddressIsIndistinguishable(t *testing.T) {
	h := newIDHarness(t, true)

	// A fresh invite each time: two people each holding their own, both typing
	// the same address. The second one's invite is not spent, because no account
	// was created (spec 0015, AC-10).
	first := h.do(t, "POST", "/v1/auth/register", h.registration(t, "a@example.com", goodPassword), nil)
	firstMail := h.mail.last(t)
	second := h.do(t, "POST", "/v1/auth/register", h.registration(t, "a@example.com", goodPassword), nil)
	secondMail := h.mail.last(t)

	if first.Code != second.Code {
		t.Errorf("statuses differ: %d then %d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("bodies differ:\n%s\n%s", first.Body, second.Body)
	}
	if !strings.Contains(secondMail.Subject, "Someone tried to register") {
		t.Errorf("the second message is %q, want the sign in instead one", secondMail.Subject)
	}
	if strings.Contains(secondMail.Body, "?token=") {
		t.Error("the second message carried a verification link")
	}
	if !strings.Contains(firstMail.Body, "?token=") {
		t.Error("the first message carried no verification link")
	}
}

// TestConcurrentRegistrationsOfOneAddressMakeOneAccount is the race half of
// AC-2: the unique index decides, not a read before the write.
func TestConcurrentRegistrationsOfOneAddressMakeOneAccount(t *testing.T) {
	h := newIDHarness(t, true)

	// Each racer holds its own invite, drawn before the race so the minting is
	// not what is being raced. Only one of the four can win the address, and the
	// other three see the same 202 a fresh registration sees.
	bodies := make([]map[string]string, 4)
	for i := range bodies {
		bodies[i] = h.registration(t, "a@example.com", goodPassword)
	}

	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = h.do(t, "POST", "/v1/auth/register", bodies[i], nil).Code
		}()
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusAccepted {
			t.Errorf("registration %d answered %d, want 202", i, c)
		}
	}
	if _, err := h.store.GetAccountByEmail(t.Context(), "a@example.com"); err != nil {
		t.Fatalf("reading the account back: %v", err)
	}
	accounts, err := h.store.ListAccounts(t.Context())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	// The bootstrap account plus exactly one registration.
	if len(accounts) != 2 {
		t.Errorf("got %d accounts, want 2", len(accounts))
	}
}

// TestRegistrationRefusesBadInput is AC-3, including that no composition rule
// beyond length is applied.
func TestRegistrationRefusesBadInput(t *testing.T) {
	tests := []struct {
		name, email, password, code string
		status                      int
	}{
		{"short password", "a@example.com", "elevenchars", "password_too_short", 422},
		{"unparseable address", "not-an-address", goodPassword, "email_invalid", 422},
		{"overlong address", strings.Repeat("a", 250) + "@example.com", goodPassword, "email_invalid", 422},
		{"all lower case letters is fine", "a@example.com", "aaaaaaaaaaaa", "", 202},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newIDHarness(t, true)
			rec := h.do(t, "POST", "/v1/auth/register", h.registration(t, tc.email, tc.password), nil)
			if rec.Code != tc.status {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if tc.code != "" && codeOf(t, rec) != tc.code {
				t.Errorf("got code %q, want %q", codeOf(t, rec), tc.code)
			}
		})
	}
}

// TestFirstRegisteredAccountIsAdmin is AC-4 through the real surface, on a
// database that already holds the bootstrap account.
func TestFirstRegisteredAccountIsAdmin(t *testing.T) {
	h := newIDHarness(t, true)
	firstCookie := h.registerAndVerify(t, "first@example.com")
	secondCookie := h.registerAndVerify(t, "second@example.com")

	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
		want   bool
	}{
		{"first", firstCookie, true},
		{"second", secondCookie, false},
	} {
		rec := h.do(t, "GET", "/v1/auth/me", nil, tc.cookie)
		var body struct {
			IsAdmin bool `json:"is_admin"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("reading me: %v", err)
		}
		if body.IsAdmin != tc.want {
			t.Errorf("%s account: is_admin=%v, want %v", tc.name, body.IsAdmin, tc.want)
		}
	}
}

// TestLinkIsSingleUseAndPurposeBound is AC-5 through the surface, including that
// all four failing cases answer in the same words.
func TestLinkIsSingleUseAndPurposeBound(t *testing.T) {
	h := newIDHarness(t, true)
	h.do(t, "POST", "/v1/auth/register", h.registration(t, "a@example.com", goodPassword), nil)
	verifyToken := linkToken(t, h.mail.last(t).Body)

	if got := h.do(t, "GET", "/v1/auth/verify?token="+verifyToken, nil, nil); got.Code != 200 {
		t.Fatalf("first use: got %d, want 200", got.Code)
	}

	// A reset link, minted for the other purpose, must not verify.
	h.do(t, "POST", "/v1/auth/forgot", map[string]string{"email": "a@example.com"}, nil)
	resetToken := linkToken(t, h.mail.last(t).Body)

	var bodies []string
	for _, tc := range []struct{ name, token string }{
		{"second use", verifyToken},
		{"unknown", "not-a-real-token"},
		{"wrong purpose", resetToken},
		{"empty", ""},
	} {
		rec := h.do(t, "GET", "/v1/auth/verify?token="+tc.token, nil, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", tc.name, rec.Code)
		}
		if code := codeOf(t, rec); code != "link_invalid" {
			t.Errorf("%s: got code %q, want link_invalid", tc.name, code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Errorf("the four refusals are not in the same words:\n%s\n%s", bodies[0], b)
		}
	}
}

// TestResendSupersedesTheLiveLink is AC-6 through the surface.
func TestResendSupersedesTheLiveLink(t *testing.T) {
	h := newIDHarness(t, true)
	h.do(t, "POST", "/v1/auth/register", h.registration(t, "a@example.com", goodPassword), nil)
	first := linkToken(t, h.mail.last(t).Body)

	if got := h.do(t, "POST", "/v1/auth/resend", map[string]string{"email": "a@example.com"}, nil); got.Code != 202 {
		t.Fatalf("resending: got %d, want 202: %s", got.Code, got.Body)
	}
	second := linkToken(t, h.mail.last(t).Body)
	if first == second {
		t.Fatal("the resend reissued the same token")
	}
	if got := h.do(t, "GET", "/v1/auth/verify?token="+first, nil, nil); got.Code != 400 {
		t.Errorf("the superseded link still worked: %d", got.Code)
	}
	if got := h.do(t, "GET", "/v1/auth/verify?token="+second, nil, nil); got.Code != 200 {
		t.Errorf("the fresh link did not work: %d", got.Code)
	}

	// An address nobody registered is answered exactly as a real one is.
	unknown := h.do(t, "POST", "/v1/auth/resend", map[string]string{"email": "nobody@example.com"}, nil)
	if unknown.Code != 202 {
		t.Errorf("an unknown address answered %d, want 202", unknown.Code)
	}
}

// TestSignInFailuresAreOneAnswer is AC-8: a wrong password, an unknown address
// and a disabled account are indistinguishable; unverified is the one exception.
func TestSignInFailuresAreOneAnswer(t *testing.T) {
	h := newIDHarness(t, true)
	h.registerAndVerify(t, "live@example.com")

	// An account registered but never verified.
	h.do(t, "POST", "/v1/auth/register", h.registration(t, "unverified@example.com", goodPassword), nil)

	// A disabled one.
	h.registerAndVerify(t, "disabled@example.com")
	off, err := h.store.GetAccountByEmail(t.Context(), "disabled@example.com")
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if err := h.store.SetAccountDisabled(t.Context(), off.ID, true); err != nil {
		t.Fatalf("disabling: %v", err)
	}

	var same []string
	for _, tc := range []struct{ name, email, password string }{
		{"wrong password", "live@example.com", "another long password"},
		{"unknown address", "nobody@example.com", goodPassword},
		{"disabled account", "disabled@example.com", goodPassword},
	} {
		rec := h.do(t, "POST", "/v1/auth/login",
			map[string]string{"email": tc.email, "password": tc.password}, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", tc.name, rec.Code)
		}
		if code := codeOf(t, rec); code != "credentials_invalid" {
			t.Errorf("%s: got code %q, want credentials_invalid", tc.name, code)
		}
		same = append(same, rec.Body.String())
	}
	for _, b := range same[1:] {
		if b != same[0] {
			t.Errorf("the three refusals differ:\n%s\n%s", same[0], b)
		}
	}

	unverified := h.do(t, "POST", "/v1/auth/login",
		map[string]string{"email": "unverified@example.com", "password": goodPassword}, nil)
	if unverified.Code != http.StatusForbidden || codeOf(t, unverified) != "email_unverified" {
		t.Errorf("unverified: got %d %q, want 403 email_unverified", unverified.Code, codeOf(t, unverified))
	}
}

// TestBootstrapAccountCannotBeSignedInTo is AC-11. The bootstrap account holds no
// password hash, and a sign in as it is refused exactly as a wrong password is.
func TestBootstrapAccountCannotBeSignedInTo(t *testing.T) {
	h := newIDHarness(t, true)
	rec := h.do(t, "POST", "/v1/auth/login",
		map[string]string{"email": "bootstrap", "password": goodPassword}, nil)
	if rec.Code != http.StatusUnauthorized || codeOf(t, rec) != "credentials_invalid" {
		t.Errorf("got %d %q, want 401 credentials_invalid", rec.Code, codeOf(t, rec))
	}
	// And its token still works, which is the whole point of leaving it alone.
	as := store.ForAuth(h.store)
	if _, err := auth.NewAuthenticator(as, as).Authenticate(t.Context(), goodToken, ""); err != nil {
		t.Errorf("the bootstrap token stopped working: %v", err)
	}
}

// TestLogoutAndDisableEndASession is AC-9 and AC-10 through the surface.
func TestLogoutAndDisableEndASession(t *testing.T) {
	h := newIDHarness(t, true)
	cookie := h.registerAndVerify(t, "a@example.com")

	if got := h.do(t, "GET", "/v1/auth/me", nil, cookie); got.Code != 200 {
		t.Fatalf("the session did not work: %d", got.Code)
	}
	rec := h.do(t, "POST", "/v1/auth/logout", nil, cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logging out: got %d, want 204", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if auth.IsSessionCookie(c.Name) && c.MaxAge >= 0 {
			t.Errorf("the cookie was not expired: MaxAge=%d", c.MaxAge)
		}
	}
	if got := h.do(t, "GET", "/v1/auth/me", nil, cookie); got.Code != 401 {
		t.Errorf("a revoked session still worked: %d", got.Code)
	}

	// A fresh session, killed by a disable instead.
	cookie = h.signIn(t, "a@example.com", goodPassword)
	acc, err := h.store.GetAccountByEmail(t.Context(), "a@example.com")
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if err := h.store.SetAccountDisabled(t.Context(), acc.ID, true); err != nil {
		t.Fatalf("disabling: %v", err)
	}
	if got := h.do(t, "GET", "/v1/auth/me", nil, cookie); got.Code != 401 {
		t.Errorf("a disabled account's session still worked: %d", got.Code)
	}
}

// TestSessionExpiryRolls is the other half of AC-9: each use pushes it forward,
// and it lapses once nothing has used it for the whole lifetime.
func TestSessionExpiryRolls(t *testing.T) {
	h := newIDHarness(t, true)
	cookie := h.registerAndVerify(t, "a@example.com")

	// Well inside the window, twice, each use pushing the expiry on.
	for range 3 {
		h.clock.Advance(20 * 24 * time.Hour)
		if got := h.do(t, "GET", "/v1/auth/me", nil, cookie); got.Code != 200 {
			t.Fatalf("the session lapsed while it was being used: %d", got.Code)
		}
	}
	// Now leave it alone for longer than the lifetime.
	h.clock.Advance(identity.SessionLifetime + time.Hour)
	if got := h.do(t, "GET", "/v1/auth/me", nil, cookie); got.Code != 401 {
		t.Errorf("an untouched session outlived its expiry: %d", got.Code)
	}
}

// TestUnverifiedAccountCannotMintOrAuthenticate is AC-15 and AC-16.
func TestUnverifiedAccountCannotMintOrAuthenticate(t *testing.T) {
	h := newIDHarness(t, true)
	cookie := h.registerAndVerify(t, "a@example.com")

	rec := h.do(t, "POST", "/v1/tokens", map[string]any{"name": "agent"}, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("minting: got %d: %s", rec.Code, rec.Body)
	}
	var minted struct{ Token string }
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("reading the mint body: %v", err)
	}

	// Reverse the verification behind the platform's back, which is the only way
	// to reach the state the gate exists for.
	if _, err := h.store.DB().ExecContext(t.Context(),
		`UPDATE accounts SET email_verified_at = NULL WHERE email = ?`, "a@example.com"); err != nil {
		t.Fatalf("unverifying: %v", err)
	}

	as := store.ForAuth(h.store)
	if _, err := auth.NewAuthenticator(as, as).Authenticate(t.Context(), minted.Token, ""); err == nil {
		t.Error("an unverified account's token still authenticated on the machine route")
	}
	// The person route refuses too, and says which refusal it is, because the
	// caller already proved they hold a live session and the useful answer is the
	// true one (AC-15). Disabled stays indistinguishable; unverified does not.
	again := h.do(t, "POST", "/v1/tokens", map[string]any{"name": "second"}, cookie)
	if again.Code != http.StatusForbidden || codeOf(t, again) != "email_unverified" {
		t.Errorf("minting unverified: got %d %q, want 403 email_unverified",
			again.Code, codeOf(t, again))
	}
	if got := h.do(t, "GET", "/v1/auth/me", nil, cookie); got.Code != http.StatusForbidden {
		t.Errorf("an unverified account's session still worked: %d", got.Code)
	}

	// And the bootstrap account, which holds no address, stays exempt.
	if _, err := auth.NewAuthenticator(as, as).Authenticate(t.Context(), goodToken, ""); err != nil {
		t.Errorf("the bootstrap token was caught by the verified gate: %v", err)
	}
}

// TestTokensAreScopedToTheirOwner is AC-13 and AC-14.
func TestTokensAreScopedToTheirOwner(t *testing.T) {
	h := newIDHarness(t, true)
	mine := h.registerAndVerify(t, "mine@example.com")
	theirs := h.registerAndVerify(t, "theirs@example.com")

	rec := h.do(t, "POST", "/v1/tokens", map[string]any{"name": "agent"}, mine)
	var minted struct{ ID, Token string }
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("reading the mint body: %v", err)
	}

	// The other account's list does not carry it.
	list := h.do(t, "GET", "/v1/tokens", nil, theirs)
	if strings.Contains(list.Body.String(), minted.ID) {
		t.Error("one account's token appeared in another's list")
	}
	// Nor does any list carry a raw value or a hash.
	mineList := h.do(t, "GET", "/v1/tokens", nil, mine)
	for _, secret := range []string{minted.Token, identity.HashSecret(minted.Token)} {
		if strings.Contains(mineList.Body.String(), secret) {
			t.Error("a token list carried a secret")
		}
	}

	// Revoking somebody else's is 404, exactly as an unknown id is.
	other := h.do(t, "DELETE", "/v1/tokens/"+minted.ID, nil, theirs)
	unknown := h.do(t, "DELETE", "/v1/tokens/tok_does_not_exist", nil, theirs)
	if other.Code != http.StatusNotFound || other.Body.String() != unknown.Body.String() {
		t.Errorf("somebody else's token id is distinguishable from an unknown one:\n%d %s\n%d %s",
			other.Code, other.Body, unknown.Code, unknown.Body)
	}

	// The owner may revoke it, and it stops working on the very next request.
	if got := h.do(t, "DELETE", "/v1/tokens/"+minted.ID, nil, mine); got.Code != http.StatusNoContent {
		t.Fatalf("revoking my own token: got %d", got.Code)
	}
	as := store.ForAuth(h.store)
	if _, err := auth.NewAuthenticator(as, as).Authenticate(t.Context(), minted.Token, ""); err == nil {
		t.Error("a revoked token still authenticated")
	}
}

// TestTokenExpiryIsBounded is the rest of AC-12.
func TestTokenExpiryIsBounded(t *testing.T) {
	h := newIDHarness(t, true)
	cookie := h.registerAndVerify(t, "a@example.com")

	for _, tc := range []struct {
		name   string
		days   int
		status int
	}{
		{"no lifetime", 0, http.StatusCreated},
		{"one day", 1, http.StatusCreated},
		{"a year", 365, http.StatusCreated},
		{"beyond a year", 366, http.StatusUnprocessableEntity},
		{"negative", -5, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(t, "POST", "/v1/tokens",
				map[string]any{"name": tc.name, "expires_in_days": tc.days}, cookie)
			if rec.Code != tc.status {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if tc.status == http.StatusUnprocessableEntity && codeOf(t, rec) != "invalid_expiry" {
				t.Errorf("got code %q, want invalid_expiry", codeOf(t, rec))
			}
		})
	}

	// A second live token by the same name is refused.
	if got := h.do(t, "POST", "/v1/tokens", map[string]any{"name": "one day"}, cookie); got.Code != http.StatusConflict {
		t.Errorf("a duplicate live name: got %d, want 409", got.Code)
	}
}

// TestAdminSurfaceNeedsAnAdminSession is AC-19 and AC-20.
func TestAdminSurfaceNeedsAnAdminSession(t *testing.T) {
	h := newIDHarness(t, true)
	admin := h.registerAndVerify(t, "admin@example.com")
	ordinary := h.registerAndVerify(t, "ordinary@example.com")

	if got := h.do(t, "GET", "/v1/admin/accounts", nil, admin); got.Code != 200 {
		t.Fatalf("the admin was refused: %d %s", got.Code, got.Body)
	}
	refused := h.do(t, "GET", "/v1/admin/accounts", nil, ordinary)
	if refused.Code != http.StatusForbidden || codeOf(t, refused) != "admin_required" {
		t.Errorf("an ordinary account: got %d %q, want 403 admin_required", refused.Code, codeOf(t, refused))
	}

	// An API token is not a session, so it is 401 rather than 403 (AC-20).
	req := httptest.NewRequest("GET", "/v1/admin/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+goodToken)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a bearer token on the admin surface: got %d, want 401", rec.Code)
	}
}

// TestAdminDisablesAndRevokes walks the three state changing admin endpoints.
func TestAdminDisablesAndRevokes(t *testing.T) {
	h := newIDHarness(t, true)
	admin := h.registerAndVerify(t, "admin@example.com")
	target := h.registerAndVerify(t, "target@example.com")

	rec := h.do(t, "POST", "/v1/tokens", map[string]any{"name": "agent"}, target)
	var minted struct{ ID, Token string }
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("reading the mint body: %v", err)
	}
	acc, err := h.store.GetAccountByEmail(t.Context(), "target@example.com")
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}

	if got := h.do(t, "DELETE", "/v1/admin/accounts/"+acc.ID+"/tokens/"+minted.ID, nil, admin); got.Code != 204 {
		t.Fatalf("revoking another account's token: got %d %s", got.Code, got.Body)
	}
	as := store.ForAuth(h.store)
	if _, err := auth.NewAuthenticator(as, as).Authenticate(t.Context(), minted.Token, ""); err == nil {
		t.Error("the revoked token still authenticated")
	}

	if got := h.do(t, "POST", "/v1/admin/accounts/"+acc.ID+"/disable", nil, admin); got.Code != 204 {
		t.Fatalf("disabling: got %d %s", got.Code, got.Body)
	}
	if got := h.do(t, "GET", "/v1/auth/me", nil, target); got.Code != 401 {
		t.Errorf("the disabled account's session survived: %d", got.Code)
	}
	if got := h.do(t, "POST", "/v1/admin/accounts/"+acc.ID+"/enable", nil, admin); got.Code != 204 {
		t.Fatalf("enabling: got %d %s", got.Code, got.Body)
	}
	h.signIn(t, "target@example.com", goodPassword)

	// An unknown account id is 404 on every one of them.
	for _, path := range []string{
		"/v1/admin/accounts/acc_nope/disable",
		"/v1/admin/accounts/acc_nope/enable",
	} {
		if got := h.do(t, "POST", path, nil, admin); got.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", path, got.Code)
		}
	}
}

// TestPasswordResetEndsEverySession is AC-28 and AC-29.
func TestPasswordResetEndsEverySession(t *testing.T) {
	const newPassword = "an entirely different password"
	h := newIDHarness(t, true)
	cookie := h.registerAndVerify(t, "a@example.com")

	// An address nobody registered still answers 202, and identically.
	known := h.do(t, "POST", "/v1/auth/forgot", map[string]string{"email": "a@example.com"}, nil)
	unknown := h.do(t, "POST", "/v1/auth/forgot", map[string]string{"email": "nobody@example.com"}, nil)
	if known.Code != 202 || unknown.Code != 202 || known.Body.String() != unknown.Body.String() {
		t.Errorf("forgot is distinguishable:\n%d %s\n%d %s", known.Code, known.Body, unknown.Code, unknown.Body)
	}

	// Only the real address received anything.
	if to := h.mail.last(t).To; to != "a@example.com" {
		t.Errorf("a message went to %q", to)
	}
	resetToken := linkToken(t, h.mail.last(t).Body)

	// A verify link presented to reset is refused in the same words as an
	// unknown one. It comes from a second, still unverified account, because a
	// verified one holds no live verification link to borrow.
	h.do(t, "POST", "/v1/auth/register", h.registration(t, "b@example.com", goodPassword), nil)
	verifyToken := linkToken(t, h.mail.last(t).Body)
	wrongPurpose := h.do(t, "POST", "/v1/auth/reset",
		map[string]string{"token": verifyToken, "password": newPassword}, nil)
	if wrongPurpose.Code != http.StatusBadRequest || codeOf(t, wrongPurpose) != "link_invalid" {
		t.Errorf("a verify link on reset: got %d %q, want 400 link_invalid", wrongPurpose.Code, codeOf(t, wrongPurpose))
	}

	// A short password is refused before the link is spent.
	if got := h.do(t, "POST", "/v1/auth/reset",
		map[string]string{"token": resetToken, "password": "too short"}, nil); got.Code != 422 {
		t.Fatalf("a short reset password: got %d, want 422", got.Code)
	}

	if got := h.do(t, "POST", "/v1/auth/reset",
		map[string]string{"token": resetToken, "password": newPassword}, nil); got.Code != http.StatusNoContent {
		t.Fatalf("resetting: got %d %s", got.Code, got.Body)
	}
	if got := h.do(t, "GET", "/v1/auth/me", nil, cookie); got.Code != 401 {
		t.Errorf("a session survived the reset: %d", got.Code)
	}
	if got := h.do(t, "POST", "/v1/auth/login",
		map[string]string{"email": "a@example.com", "password": goodPassword}, nil); got.Code != 401 {
		t.Errorf("the old password still worked: %d", got.Code)
	}
	h.signIn(t, "a@example.com", newPassword)

	// The link is spent, so it cannot be used again.
	if got := h.do(t, "POST", "/v1/auth/reset",
		map[string]string{"token": resetToken, "password": newPassword}, nil); got.Code != 400 {
		t.Errorf("the reset link worked twice: %d", got.Code)
	}
}

// TestFailedSignInsThrottle is AC-23, including that a correct password resets
// the counter.
func TestFailedSignInsThrottle(t *testing.T) {
	h := newIDHarness(t, true)
	h.registerAndVerify(t, "a@example.com")

	wrong := map[string]string{"email": "a@example.com", "password": "another long password"}
	for i := range 5 {
		if got := h.do(t, "POST", "/v1/auth/login", wrong, nil); got.Code != 401 {
			t.Fatalf("attempt %d: got %d, want 401", i, got.Code)
		}
	}
	locked := h.do(t, "POST", "/v1/auth/login", wrong, nil)
	if locked.Code != http.StatusTooManyRequests || codeOf(t, locked) != "rate_limited" {
		t.Fatalf("after five failures: got %d %q, want 429 rate_limited", locked.Code, codeOf(t, locked))
	}
	// Even the right password is refused while the window is open.
	if got := h.do(t, "POST", "/v1/auth/login",
		map[string]string{"email": "a@example.com", "password": goodPassword}, nil); got.Code != 429 {
		t.Errorf("inside the window: got %d, want 429", got.Code)
	}

	h.clock.Advance(31 * time.Second)
	h.signIn(t, "a@example.com", goodPassword)

	// The counter reset, so a fresh failure is a 401 again rather than a 429.
	if got := h.do(t, "POST", "/v1/auth/login", wrong, nil); got.Code != 401 {
		t.Errorf("after a successful sign in: got %d, want 401", got.Code)
	}
}

// TestClientBucketIsShared is AC-24: the four unauthenticated endpoints draw on
// one bucket keyed by the client address the ingress reported.
func TestClientBucketIsShared(t *testing.T) {
	h := newIDHarness(t, true)

	send := func(client string) int {
		req := httptest.NewRequest("POST", "/v1/auth/forgot",
			strings.NewReader(`{"email":"nobody@example.com"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "10.0.0.1, "+client)
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, req)
		return rec.Code
	}

	for i := range 10 {
		if got := send("100.64.0.5"); got != 202 {
			t.Fatalf("call %d: got %d, want 202", i, got)
		}
	}
	if got := send("100.64.0.5"); got != http.StatusTooManyRequests {
		t.Errorf("the eleventh call: got %d, want 429", got)
	}
	// A different client has its own bucket, so one caller cannot lock the rest out.
	if got := send("100.64.0.9"); got != 202 {
		t.Errorf("another client: got %d, want 202", got)
	}
	// And a token comes back on the refill schedule.
	h.clock.Advance(7 * time.Second)
	if got := send("100.64.0.5"); got != 202 {
		t.Errorf("after a refill: got %d, want 202", got)
	}
}

// TestMailFailureDoesNotFailTheRequest is AC-25.
func TestMailFailureDoesNotFailTheRequest(t *testing.T) {
	h := newIDHarness(t, true)
	h.mail.fail = true

	rec := h.do(t, "POST", "/v1/auth/register", h.registration(t, "a@example.com", goodPassword), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202: %s", rec.Code, rec.Body)
	}
	if _, err := h.store.GetAccountByEmail(t.Context(), "a@example.com"); err != nil {
		t.Errorf("the account was not created: %v", err)
	}
	if h.mail.count() == 0 {
		t.Error("no send was attempted")
	}
}

// TestNoMailerRefusesOnlyTheMailEndpoints is AC-26.
func TestNoMailerRefusesOnlyTheMailEndpoints(t *testing.T) {
	h := newIDHarness(t, false)

	// Registration carries a live invite, because the invite gate sits ahead of
	// the mailer check: without one this endpoint would answer invite_invalid
	// and never reach the case under test (spec 0015, Key invariants).
	for _, tc := range []struct{ path, body string }{
		{"/v1/auth/register", `{"invite":"` + h.invite(t) + `","email":"a@example.com","password":"a long enough password"}`},
		{"/v1/auth/resend", `{"email":"a@example.com"}`},
		{"/v1/auth/forgot", `{"email":"a@example.com"}`},
	} {
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || codeOf(t, rec) != "mail_unavailable" {
			t.Errorf("%s: got %d %q, want 503 mail_unavailable", tc.path, rec.Code, codeOf(t, rec))
		}
	}

	// The machine route is untouched.
	as := store.ForAuth(h.store)
	if _, err := auth.NewAuthenticator(as, as).Authenticate(t.Context(), goodToken, ""); err != nil {
		t.Errorf("the bootstrap token stopped working with no mailer: %v", err)
	}
}

// TestNoResponseCarriesASecret is AC-27, swept across the whole surface: no raw
// token, session id, link token or password appears in any body or audit row.
func TestNoResponseCarriesASecret(t *testing.T) {
	h := newIDHarness(t, true)
	cookie := h.registerAndVerify(t, "a@example.com")
	rec := h.do(t, "POST", "/v1/tokens", map[string]any{"name": "agent"}, cookie)
	var minted struct{ Token string }
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("reading the mint body: %v", err)
	}

	secrets := []string{goodPassword, cookie.Value, minted.Token}

	// Every response the surface gives, except the one mint that is allowed to
	// carry its token exactly once.
	bodies := []string{
		h.do(t, "GET", "/v1/auth/me", nil, cookie).Body.String(),
		h.do(t, "GET", "/v1/tokens", nil, cookie).Body.String(),
		h.do(t, "GET", "/v1/admin/accounts", nil, cookie).Body.String(),
		h.do(t, "POST", "/v1/auth/login",
			map[string]string{"email": "a@example.com", "password": goodPassword}, nil).Body.String(),
	}
	for _, body := range bodies {
		for _, secret := range secrets {
			if strings.Contains(body, secret) {
				t.Errorf("a response carried a secret: %s", body)
			}
		}
	}

	// And no audit row does either.
	rows, err := h.store.DB().QueryContext(t.Context(),
		`SELECT action, COALESCE(target_id, ''), COALESCE(reason, '') FROM audit_log`)
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var action, target, reason string
		if err := rows.Scan(&action, &target, &reason); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		seen++
		for _, secret := range secrets {
			for _, field := range []string{action, target, reason} {
				if strings.Contains(field, secret) {
					t.Errorf("an audit row carried a secret: %s %s %s", action, target, reason)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	if seen == 0 {
		t.Error("nothing was audited across a register, sign in, mint and admin read")
	}
}

// TestEveryPrivilegedActionIsAudited is AC-22.
func TestEveryPrivilegedActionIsAudited(t *testing.T) {
	h := newIDHarness(t, true)
	admin := h.registerAndVerify(t, "admin@example.com")

	rec := h.do(t, "POST", "/v1/tokens", map[string]any{"name": "agent"}, admin)
	var minted struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("reading the mint body: %v", err)
	}
	h.do(t, "DELETE", "/v1/tokens/"+minted.ID, nil, admin)
	h.do(t, "GET", "/v1/admin/accounts", nil, admin)
	h.do(t, "POST", "/v1/auth/login",
		map[string]string{"email": "admin@example.com", "password": "the wrong password"}, nil)

	want := map[string]bool{
		auth.ActionTokenMint:   false,
		auth.ActionTokenRevoke: false,
		auth.ActionAdmin:       false,
		auth.ActionLogin:       false,
	}
	rows, err := h.store.DB().QueryContext(t.Context(), `SELECT DISTINCT action FROM audit_log`)
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		if _, ok := want[action]; ok {
			want[action] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	for action, found := range want {
		if !found {
			t.Errorf("%q was never audited", action)
		}
	}
}
