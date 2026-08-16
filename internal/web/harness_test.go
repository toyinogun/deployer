package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/logs"
	"github.com/toyinogun/deployer/internal/store"
	"github.com/toyinogun/deployer/internal/suspend"
)

// The pages run over a real SQLite file, the real identity service and the real
// authenticator, because the session gate, the rate limit and the audit rows are
// exactly what these tests are about. Only the two reads the package declares
// interfaces for are faked: they stand in for the store's app queries and for
// Kubernetes, neither of which has a fixture cheap enough to be worth the
// coupling here (spec 0013, AC-4).

const (
	testPassword  = "a long enough password"
	testAppDomain = "apps.example.test"
	// The public console hostname. Requests these tests build carry httptest's
	// own Host, so the whole existing suite runs on the "any other host" branch,
	// which is AC-4 held by every test in the package at once.
	testConsoleHost = "console.apps.example.test"
	// Derived from the host exactly as config does it, so a test cannot pass
	// against a pair the platform could never hold (spec 0022, AC-8).
	testConsoleURL = "https://" + testConsoleHost
	// The public deploy hostname and its address. A different name from the
	// console on purpose: the two endpoints the apps page shows are on the
	// deploy host, and the pages themselves never are (spec 0022, AC-9).
	testMCPHost = "mcp.apps.example.test"
	testMCPURL  = "https://" + testMCPHost
)

// csrfInPage pulls the synchroniser token out of a rendered form, which is the
// only way a test can get one: it is derived at render and never stored.
var csrfInPage = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

// fakeData is every read a page makes. Each field is what the next call answers
// with, so a test states the world it wants rather than seeding six tables.
type fakeData struct {
	summaries []store.AppSummary
	// lastPage records the paging arguments the apps list asked for, so a test
	// can pin the cursor and the size rather than trusting the rendered rows.
	lastPage store.Page

	apps       map[string]store.App
	releases   map[string]store.Release
	byApp      []store.Release
	deployment store.Deployment
	noDeploy   bool
	config     []store.ConfigEntry
	ranConfig  map[string]string

	// The per account cap spec 0016 added. appsHeld is what the usage line and
	// the at cap notice are rendered from, and perAccountApps is the admin
	// listing's grouped count, keyed by account id.
	appsHeld       int
	perAccountApps map[string]int

	err error
}

func (f *fakeData) CountLiveAppsByAccount(_ context.Context, _ string) (int, error) {
	return f.appsHeld, f.err
}

func (f *fakeData) CountLiveAppsPerAccount(_ context.Context) (map[string]int, error) {
	return f.perAccountApps, f.err
}

func (f *fakeData) ListAppSummaryPage(_ context.Context, _ string, page store.Page) ([]store.AppSummary, error) {
	f.lastPage = page
	return f.summaries, f.err
}

func (f *fakeData) GetAppBySlug(_ context.Context, slug string) (store.App, error) {
	app, ok := f.apps[slug]
	if !ok {
		return store.App{}, store.ErrNotFound
	}
	return app, nil
}

func (f *fakeData) GetRelease(_ context.Context, id string) (store.Release, error) {
	rel, ok := f.releases[id]
	if !ok {
		return store.Release{}, store.ErrNotFound
	}
	return rel, nil
}

func (f *fakeData) GetLatestDeploymentForApp(_ context.Context, _ string) (store.Deployment, error) {
	if f.noDeploy {
		return store.Deployment{}, store.ErrNotFound
	}
	return f.deployment, nil
}

func (f *fakeData) ListReleasesByApp(_ context.Context, _ string, _ store.Page) ([]store.Release, error) {
	return f.byApp, nil
}

func (f *fakeData) ListConfigForResponse(_ context.Context, _ string) ([]store.ConfigEntry, error) {
	return f.config, nil
}

func (f *fakeData) CurrentReleaseConfig(_ context.Context, _ string) (map[string]string, error) {
	return f.ranConfig, nil
}

// fakePods is the app output read, standing in for Kubernetes.
type fakePods struct {
	pods []logs.PodStatus
	out  string
	err  error
}

func (f *fakePods) PodsForApp(_ context.Context, _, _ string) ([]logs.PodStatus, error) {
	return f.pods, f.err
}

func (f *fakePods) PodLog(_ context.Context, _, _ string, _ int, _ bool) (string, error) {
	return f.out, nil
}

// recorder keeps every audit row the pages wrote, because several acceptance
// criteria are about the row and not about the response.
type recorder struct {
	mu   sync.Mutex
	rows []auth.Audit
}

func (rec *recorder) RecordAudit(_ context.Context, a auth.Audit) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.rows = append(rec.rows, a)
	return nil
}

func (rec *recorder) all() []auth.Audit {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]auth.Audit(nil), rec.rows...)
}

// last returns the newest row for an action, and whether there was one.
func (rec *recorder) last(action string) (auth.Audit, bool) {
	rows := rec.all()
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Action == action {
			return rows[i], true
		}
	}
	return auth.Audit{}, false
}

// hasReason reports whether any row for an action carries a reason, for the
// refusals that re render a page and so write a second row after their own.
func (rec *recorder) hasReason(action, reason string) bool {
	for _, row := range rec.all() {
		if row.Action == action && row.Reason == reason {
			return true
		}
	}
	return false
}

// mailbox is the sender, kept so a test can read a verification link the way a
// person reads one out of their inbox.
type mailbox struct {
	mu   sync.Mutex
	sent []string
}

func (m *mailbox) Send(_ context.Context, _, _, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, body)
	return nil
}

func (m *mailbox) latest(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("no message was sent")
	}
	return m.sent[len(m.sent)-1]
}

// harness is the page surface over a real database, ready to be driven with
// requests.
type harness struct {
	mux   *http.ServeMux
	srv   *Server
	data  *fakeData
	pods  *fakePods
	audit *recorder
	mail  *mailbox
	clock *ids.FixedClock
	store *store.Store
	// scaler stands in for the cluster the suspension path scales through. A test
	// that cares sets refuse before acting; every other test never looks at it.
	scaler *fakeScaler
	// backups stands in for the backup service. Configured by default, so a page
	// test that is not about backups never renders the unconfigured notice.
	backups *fakeBackups
}

// fakeScaler records what the suspension path asked the cluster for, and can
// refuse one namespace so the partial outcome has a way to happen.
type fakeScaler struct {
	scaled map[string]int32
	refuse map[string]bool
}

func (f *fakeScaler) ScaleWorkload(_ context.Context, namespace, _ string, replicas int32) error {
	if f.refuse[namespace] {
		return errors.New("the cluster refused this namespace")
	}
	f.scaled[namespace] = replicas
	return nil
}

// newHarness builds the pages. pods nil is the no cluster credential case the
// logs page renders as an empty state rather than refusing to start.
func newHarness(t *testing.T, pods *fakePods) *harness {
	t.Helper()
	clock := &ids.FixedClock{T: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)}
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
	authenticator := auth.NewAuthenticator(as, as).WithSessions(as, identity.SessionLifetime)
	box := &mailbox{}
	// A cheap hasher: this suite signs in repeatedly and paying the production
	// key derivation each time buys nothing here. The real parameters are pinned
	// in internal/identity's own tests.
	svc := identity.NewService(store.ForIdentity(st), box, clock, identity.Options{
		ConsoleURL: testConsoleURL,
		Hasher:     identity.NewHasherWith(2, 64, 1),
	})

	h := &harness{
		data:  &fakeData{apps: map[string]store.App{}, releases: map[string]store.Release{}},
		pods:  pods,
		audit: &recorder{},
		mail:  box,
		clock: clock,
		store: st,
	}
	var podsIface Pods
	if pods != nil {
		podsIface = pods
	}
	h.scaler = &fakeScaler{scaled: map[string]int32{}, refuse: map[string]bool{}}
	h.backups = &fakeBackups{configured: true}
	h.srv = New(svc, authenticator, h.audit, h.data, podsIface,
		suspend.New(store.ForSuspend(st), svc, h.scaler, h.audit), h.backups, st, Options{
			MCPURL:         testMCPURL,
			AppDomain:      testAppDomain,
			CSRFKey:        []byte("a test csrf key"),
			SecretLiterals: []string{"the-platform-credential"},
			HasMailer:      true,
			// Well clear of what these tests list, so a page test that is not about
			// the cap never renders the at cap notice (spec 0016).
			MaxAppsPerAccount: 10,
			ConsoleHost:       testConsoleHost,
			ConsoleURL:        testConsoleURL,
			TrustedHosts:      []string{testConsoleHost, testMCPHost},
		})
	h.mux = http.NewServeMux()
	h.srv.Register(h.mux)
	return h
}

// get runs one GET, optionally signed in. Nil cookies are skipped, so a caller
// can pass one it may or may not have, and a caller that needs to arrive as a
// browser does can pass both a session and a pre authentication cookie.
func (h *harness) get(t *testing.T, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// withoutCSRF blanks the pre authentication token out of a rendered page.
//
// Two renders of the same form carry different tokens whenever they come from
// different nonces, so a test comparing two bodies has to take it out or it
// measures the randomness rather than what the page revealed. The token is
// random per nonce and tells a reader nothing about the account, which is why
// blanking it is safe rather than hiding a real difference.
func withoutCSRF(body string) string {
	return csrfInPage.ReplaceAllString(body, `name="csrf" value="TOKEN"`)
}

// preAuthPageFor maps a guarded pre authentication post onto the page whose GET
// sets the nonce cookie it needs. /resend is the one that differs: it has no GET
// route of its own, so its cookie comes off /unverified (spec 0019, AC-1).
var preAuthPageFor = map[string]string{
	"/login":    "/login",
	"/register": "/register",
	"/forgot":   "/forgot",
	"/reset":    "/reset",
	"/resend":   "/unverified",
}

// post runs one form POST the way a browser does, which since spec 0019 means
// visiting the page first: a post to a guarded path with no session cookie and
// no token of its own picks up the nonce cookie and the hidden field from a GET,
// so the ~25 call sites that predate the guard keep working untouched.
//
// A test that wants a post without the pre authentication pair, to prove the
// refusal or the origin check, calls postRaw directly.
func (h *harness) post(t *testing.T, path string, form url.Values, cookie *http.Cookie,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	page, guarded := preAuthPageFor[path]
	if !guarded || cookie != nil || form.Get(csrfField) != "" {
		return h.postRaw(t, path, form, headers, cookie)
	}
	nonce, token := h.preAuthToken(t, page)
	signed := url.Values{}
	for k, v := range form {
		signed[k] = v
	}
	signed.Set(csrfField, token)
	return h.postRaw(t, path, signed, headers, nonce)
}

// preAuthToken does the GET a browser does before a pre authentication post,
// and returns the pair it comes away with: the nonce cookie and the token the
// form renders. They only work together, which is the whole point of the
// mechanism, so the helper hands back both or fails.
func (h *harness) preAuthToken(t *testing.T, page string) (*http.Cookie, string) {
	t.Helper()
	rec := h.get(t, page, nil)
	match := csrfInPage.FindStringSubmatch(rec.Body.String())
	if match == nil {
		t.Fatalf("no csrf field on %s: %s", page, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if strings.HasSuffix(c.Name, preCSRFCookiePlain) && c.Value != "" {
			return c, match[1]
		}
	}
	t.Fatalf("%s set no pre authentication cookie", page)
	return nil, ""
}

// postRaw runs one form POST exactly as given, adding nothing. Headers are the
// ones a same origin browser sends, so a test that wants a cross site post
// overrides them explicitly. An empty header value in headers deletes it rather
// than sending it blank. Nil cookies are skipped, so a caller can pass one it
// may or may not have.
func (h *harness) postRaw(t *testing.T, path string, form url.Values,
	headers map[string]string, cookies ...*http.Cookie,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", testConsoleURL)
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		// Host is not an ordinary header on a server request: the mux and the
		// origin check both read r.Host, and Header.Set would leave that alone.
		if http.CanonicalHeaderKey(k) == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// invite mints one live invite straight through the store and returns the raw
// code. Registration is invite only, and only an admin can issue one, so this is
// how a test gets the first person through the door: the same pair of writes the
// boot time bootstrap makes on an empty database (spec 0015, AC-13).
func (h *harness) invite(t *testing.T) string {
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

// registration is one registration form carrying a fresh invite, which every
// successful registration now needs (spec 0015, AC-1).
func (h *harness) registration(t *testing.T, email string) url.Values {
	t.Helper()
	return url.Values{
		"invite": {h.invite(t)}, "email": {email},
		"password": {testPassword}, "display_name": {"Someone"},
	}
}

// signIn walks an address from nothing to a live session over the pages
// themselves: register, click the mailed link, sign in. The first account the
// platform ever registers is its administrator, so the caller controls who is
// admin by the order it signs people in.
func (h *harness) signIn(t *testing.T, email string) *http.Cookie {
	t.Helper()
	if rec := h.post(t, "/register", h.registration(t, email), nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("registering %s: got %d, want 200", email, rec.Code)
	}
	token := linkToken(t, h.mail.latest(t))
	if rec := h.get(t, "/verify?token="+url.QueryEscape(token), nil); rec.Code != http.StatusOK {
		t.Fatalf("verifying %s: got %d, want 200", email, rec.Code)
	}

	return h.login(t, email)
}

// login signs an already registered address in again, for the tests that need a
// second session rather than a second account.
func (h *harness) login(t *testing.T, email string) *http.Cookie {
	t.Helper()
	rec := h.post(t, "/login", url.Values{"email": {email}, "password": {testPassword}}, nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("signing in %s: got %d, want 303: %s", email, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if auth.IsSessionCookie(c.Name) && c.Value != "" {
			return c
		}
	}
	t.Fatalf("signing in %s set no session cookie", email)
	return nil
}

// csrfFor reads a live synchroniser token off a page the session can render.
func (h *harness) csrfFor(t *testing.T, cookie *http.Cookie) string {
	t.Helper()
	rec := h.get(t, "/apps", cookie)
	match := csrfInPage.FindStringSubmatch(rec.Body.String())
	if match == nil {
		t.Fatal("no csrf field on a signed in page")
	}
	return match[1]
}

// linkToken pulls the raw token out of a mailed link, the way a browser would.
func linkToken(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "?token=")
	if !ok {
		t.Fatalf("no link in the message: %q", body)
	}
	token, _, _ := strings.Cut(rest, "\n")
	return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(token), ">)\"'"))
}

// ownApp registers a stored app against an account and returns it.
func (h *harness) ownApp(accountID, slug string) store.App {
	app := store.App{ID: "app_" + slug, AccountID: accountID, Name: slug, Slug: slug}
	h.data.apps[slug] = app
	return app
}

// accountID resolves who a session cookie belongs to.
func (h *harness) accountID(t *testing.T, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/apps", nil)
	req.AddCookie(cookie)
	account, _, ok := h.srv.currentSession(req)
	if !ok {
		t.Fatal("the session cookie does not resolve")
	}
	return account.ID
}

// ptr is the pointer the store's nullable columns are typed with.
func ptr[T any](v T) *T { return &v }
