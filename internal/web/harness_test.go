package web

import (
	"context"
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
)

// The pages run over a real SQLite file, the real identity service and the real
// authenticator, because the session gate, the rate limit and the audit rows are
// exactly what these tests are about. Only the two reads the package declares
// interfaces for are faked: they stand in for the store's app queries and for
// Kubernetes, neither of which has a fixture cheap enough to be worth the
// coupling here (spec 0013, AC-4).

const (
	testPassword  = "a long enough password"
	testPublicURL = "https://deploy.example.test"
	testAppDomain = "apps.example.test"
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

	err error
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
		PublicURL: testPublicURL,
		Hasher:    identity.NewHasherWith(2, 64, 1),
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
	h.srv = New(svc, authenticator, h.audit, h.data, podsIface, Options{
		PublicURL:      testPublicURL,
		AppDomain:      testAppDomain,
		CSRFKey:        []byte("a test csrf key"),
		SecretLiterals: []string{"the-platform-credential"},
		HasMailer:      true,
	})
	h.mux = http.NewServeMux()
	h.srv.Register(h.mux)
	return h
}

// get runs one GET, optionally signed in.
func (h *harness) get(t *testing.T, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// post runs one form POST. Headers are the ones a same origin browser sends, so
// a test that wants a cross site post overrides them explicitly. An empty header
// value in headers deletes it rather than sending it blank.
func (h *harness) post(t *testing.T, path string, form url.Values, cookie *http.Cookie,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", testPublicURL)
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
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
		if c.Name == auth.SessionCookie && c.Value != "" {
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
