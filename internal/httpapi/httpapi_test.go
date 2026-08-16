// Package httpapi_test drives the real handlers over a real SQLite file and a
// real upload directory. Nothing is mocked: the auth, the audit rows, and the
// single use redemption under test are the ones that will run in the cluster.
package httpapi_test

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/httpapi"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store"
	"github.com/toyinogun/deployer/internal/uploads"
)

const (
	goodToken   = "dpl_a_working_token"
	maxTestSize = 4096
	// Small on purpose, so the cap is reachable in a test without writing three
	// tarballs first (spec 0022, AC-17).
	maxTestUnclaimed = 2
	// The two public hostnames. Requests these tests build carry httptest's own
	// Host, so the existing suite runs on the default pattern, which is what
	// keeps the deploy host registration opt in rather than a rename.
	testConsoleHost = "console.apps.example.test"
	testMCPHost     = "mcp.apps.example.test"
)

// harness is a running API over a fresh database and upload directory.
type harness struct {
	mux     *http.ServeMux
	store   *store.Store
	uploads *uploads.Service
	dir     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(store.Options{Path: filepath.Join(dir, "deployer.db")})
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
		t.Fatalf("seeding: %v", err)
	}

	uploadDir := filepath.Join(dir, "uploads")
	svc := uploads.NewService(store.ForUploads(st), uploadDir, maxTestSize, maxTestUnclaimed, nil)
	mux := http.NewServeMux()
	limiter := identity.NewLimiter(ids.SystemClock{}, identity.DeployPathSettings())
	httpapi.New(auth.NewAuthenticator(as, as).WithLockout(limiter), as, svc, limiter, httpapi.Options{
		MaxBytes:     maxTestSize,
		MCPHost:      testMCPHost,
		TrustedHosts: []string{testConsoleHost, testMCPHost},
	}).Register(mux, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A stand in for the real MCP handler. This package's tests are about
		// where /mcp is registered, never about what it answers.
		w.WriteHeader(http.StatusTeapot)
	}))
	// The console's own catch all, which internal/web registers in the running
	// process. It is here because AC-3 is about what these routes do on that
	// hostname, and a mux without it is not the mux the platform serves: a
	// hostless pattern would then answer a console request, which is the exact
	// thing the console's opt in registration exists to prevent.
	mux.HandleFunc(testConsoleHost+"/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return &harness{mux: mux, store: st, uploads: svc, dir: uploadDir}
}

// do runs one request against the handlers.
func (h *harness) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// post builds an upload request carrying the given body and bearer token.
func post(t *testing.T, token string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.ContentLength = int64(len(body))
	return req
}

// tarball returns a gzip stream of the given size, which is enough for the
// upload endpoint: it checks framing and size, never the archive itself.
func tarball(t *testing.T, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(payload)); err != nil {
		t.Fatalf("writing gzip: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

// oversized returns a gzip body comfortably past the cap. The payload is
// random, because repetitive text compresses away to nothing and would not
// exercise the limit at all.
func oversized(t *testing.T) []byte {
	t.Helper()
	payload := make([]byte, maxTestSize*4)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("generating a payload: %v", err)
	}
	body := tarball(t, string(payload))
	if len(body) <= maxTestSize {
		t.Fatalf("the test body compressed to %d bytes, which is under the cap", len(body))
	}
	return body
}

// filesOnVolume counts what is actually on the upload directory, which is the
// only way to tell a refusal that wrote nothing from one that wrote and then
// deleted.
func (h *harness) filesOnVolume(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(h.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("reading the upload directory: %v", err)
	}
	return len(entries)
}

// uploadRows counts the rows in the uploads table.
func (h *harness) uploadRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.store.DB().QueryRow(`SELECT COUNT(*) FROM uploads`).Scan(&n); err != nil {
		t.Fatalf("counting uploads: %v", err)
	}
	return n
}

// expireUpload moves an upload's expiry into the past. The window is an hour,
// which no test waits out, and every path that reads expiry reads this column
// rather than the clock the row was written by.
func (h *harness) expireUpload(t *testing.T, id string) {
	t.Helper()
	_, err := h.store.DB().Exec(
		`UPDATE uploads SET expires_at = '2000-01-01T00:00:00Z' WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("expiring upload %s: %v", id, err)
	}
}

// referenceUpload writes an app and a deployment naming the upload, which is
// what makes the sweep leave its row alone. Raw SQL rather than the store's own
// methods, because what is under test is the sweep's query rather than the write
// path that produced the row.
func (h *harness) referenceUpload(t *testing.T, uploadID string) {
	t.Helper()
	var accountID string
	err := h.store.DB().QueryRow(`SELECT account_id FROM uploads WHERE id = ?`, uploadID).Scan(&accountID)
	if err != nil {
		t.Fatalf("reading the upload's account: %v", err)
	}
	const now = "2026-01-01T00:00:00Z"
	if _, err := h.store.DB().Exec(
		`INSERT INTO apps (id, account_id, name, slug, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"app_sweeptest", accountID, "sweeptest", "sweeptest", now, now); err != nil {
		t.Fatalf("inserting the app: %v", err)
	}
	if _, err := h.store.DB().Exec(
		`INSERT INTO deployments (id, app_id, account_id, upload_id, state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?)`,
		"dep_sweeptest", "app_sweeptest", accountID, uploadID, now, now); err != nil {
		t.Fatalf("inserting the deployment: %v", err)
	}
}

// auditRows counts audit_log rows matching an action and outcome.
func (h *harness) auditRows(t *testing.T, action, outcome string) int {
	t.Helper()
	var n int
	err := h.store.DB().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = ? AND outcome = ?`, action, outcome).Scan(&n)
	if err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	return n
}

// covers AC-2: a good token, a gzip body, the id and expiry back, and nothing else.
func TestCreateUploadAcceptsAGoodBody(t *testing.T) {
	h := newHarness(t)
	body := tarball(t, "some source")

	rec := h.do(t, post(t, goodToken, body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if got["upload_id"] == "" || got["expires_at"] == "" {
		t.Errorf("response = %v, want an upload_id and an expires_at", got)
	}
	// Nothing else crosses the boundary: not the path, not the hash, not the size.
	if len(got) != 2 {
		t.Errorf("response = %v, want exactly upload_id and expires_at", got)
	}
	if h.auditRows(t, auth.ActionUpload, "allowed") != 1 {
		t.Error("want one allowed audit row for the upload")
	}

	// The file landed under the upload directory, named after its id, so nothing
	// the caller sent decided where it went.
	up, err := h.uploads.Get(t.Context(), got["upload_id"])
	if err != nil {
		t.Fatalf("reading the upload back: %v", err)
	}
	if want := filepath.Join(h.dir, got["upload_id"]); up.Path != want {
		t.Errorf("path = %q, want %q", up.Path, want)
	}
	if up.SizeBytes != int64(len(body)) {
		t.Errorf("size = %d, want %d", up.SizeBytes, len(body))
	}
}

// covers AC-2, AC-19: an absent, unknown or revoked token gets 401 and one
// denial row with a null account.
func TestCreateUploadRefusesBadTokens(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"no token at all", ""},
		{"an unknown token", "dpl_never_minted"},
		{"a token that is only the right prefix", goodToken[:8]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			rec := h.do(t, post(t, tc.token, tarball(t, "source")))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if h.auditRows(t, auth.ActionUpload, "denied") != 1 {
				t.Error("want one denied audit row")
			}
			var accountID *string
			err := h.store.DB().QueryRow(
				`SELECT account_id FROM audit_log WHERE outcome = 'denied'`).Scan(&accountID)
			if err != nil {
				t.Fatalf("reading the denial row: %v", err)
			}
			if accountID != nil {
				t.Errorf("account_id = %q, want null on a denial", *accountID)
			}
		})
	}
}

func TestCreateUploadRefusesARevokedToken(t *testing.T) {
	h := newHarness(t)
	// Rotating the bootstrap token revokes the old one, which is the only
	// revocation path this slice has.
	if err := auth.Bootstrap(t.Context(), store.ForAuth(h.store), "dpl_the_new_token"); err != nil {
		t.Fatalf("rotating: %v", err)
	}

	rec := h.do(t, post(t, goodToken, tarball(t, "source")))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a revoked token", rec.Code)
	}
}

// covers AC-2: over the cap is 413, and the body is refused rather than stored.
func TestCreateUploadRefusesAnOversizedBody(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, post(t, goodToken, oversized(t)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	var n int
	if err := h.store.DB().QueryRow(`SELECT COUNT(*) FROM uploads`).Scan(&n); err != nil {
		t.Fatalf("counting uploads: %v", err)
	}
	if n != 0 {
		t.Errorf("uploads = %d, want the oversized body to have recorded nothing", n)
	}
}

// A body that lies about its length is still refused, by the reader rather than
// by the declared Content-Length.
func TestCreateUploadRefusesAnUndeclaredOversizedBody(t *testing.T) {
	h := newHarness(t)
	req := post(t, goodToken, oversized(t))
	req.ContentLength = -1

	rec := h.do(t, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// covers AC-2: a body that was never gzip is 400.
func TestCreateUploadRefusesSomethingThatIsNotGzip(t *testing.T) {
	h := newHarness(t)

	rec := h.do(t, post(t, goodToken, []byte("plain text, not an archive")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// upload posts a good body and returns the new upload's id.
func (h *harness) upload(t *testing.T) string {
	t.Helper()
	rec := h.do(t, post(t, goodToken, tarball(t, "the source")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload failed: %d %s", rec.Code, rec.Body)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return got["upload_id"]
}

// fetch asks for an upload with a fetch token.
func (h *harness) fetch(t *testing.T, id, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/uploads/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return h.do(t, req)
}

// covers AC-8: the fetch token works once, and the second attempt is refused.
func TestFetchUploadIsSingleUse(t *testing.T) {
	h := newHarness(t)
	id := h.upload(t)
	token, err := h.uploads.MintFetchToken(t.Context(), id)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}

	first := h.fetch(t, id, token)
	if first.Code != http.StatusOK {
		t.Fatalf("first fetch = %d, want 200: %s", first.Code, first.Body)
	}
	body, err := io.ReadAll(first.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	if !bytes.Equal(body[:2], []byte{0x1f, 0x8b}) {
		t.Error("the fetched body is not the gzip stream that was uploaded")
	}

	second := h.fetch(t, id, token)
	if second.Code != http.StatusConflict {
		t.Errorf("second fetch = %d, want 409", second.Code)
	}
}

func TestFetchUploadRefusesAnUnknownToken(t *testing.T) {
	h := newHarness(t)
	id := h.upload(t)

	rec := h.fetch(t, id, "not-a-real-fetch-token")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// A token that unlocks a different upload than the one asked for is told the
// same thing as an unknown one.
func TestFetchUploadRefusesATokenForAnotherUpload(t *testing.T) {
	h := newHarness(t)
	mine := h.upload(t)
	theirs := h.upload(t)
	token, err := h.uploads.MintFetchToken(t.Context(), theirs)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}

	rec := h.fetch(t, mine, token)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// Minting again after a redemption gives a working token, so a retried build is
// not stuck with a spent one.
func TestMintFetchTokenClearsAPreviousRedemption(t *testing.T) {
	h := newHarness(t)
	id := h.upload(t)

	first, err := h.uploads.MintFetchToken(t.Context(), id)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if rec := h.fetch(t, id, first); rec.Code != http.StatusOK {
		t.Fatalf("first fetch = %d", rec.Code)
	}

	second, err := h.uploads.MintFetchToken(t.Context(), id)
	if err != nil {
		t.Fatalf("re-minting: %v", err)
	}
	if rec := h.fetch(t, id, second); rec.Code != http.StatusOK {
		t.Errorf("fetch with a re-minted token = %d, want 200", rec.Code)
	}
	// The old token is dead, so a leaked one does not come back to life.
	if rec := h.fetch(t, id, first); rec.Code == http.StatusOK {
		t.Error("the previous fetch token still works after re-minting")
	}
}
