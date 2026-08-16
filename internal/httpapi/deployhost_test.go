package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
)

// onHost runs one request against the mux with an explicit Host, which is the
// only thing that tells the deploy host apart from the default pattern.
func (h *harness) onHost(t *testing.T, method, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://"+host+path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// TestTheDeployHostAnswersOnlyTheDeployRoutes is AC-2 and AC-4. Registration
// under the deploy host is opt in: the upload endpoint and /mcp answer there, and
// one catch all answers 404 for everything else, including the single use fetch
// the build's init container reads.
func TestTheDeployHostAnswersOnlyTheDeployRoutes(t *testing.T) {
	// covers: AC-2, AC-4
	t.Parallel()
	h := newHarness(t)

	// Absent from the deploy host. The fetch route is the load bearing one: it
	// is the init container's, reached on cluster DNS, and publishing it would
	// put the single use token on the open internet (AC-4).
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/uploads/upl_1"},
		{http.MethodGet, "/v1/auth/me"},
		{http.MethodPost, "/v1/auth/login"},
		{http.MethodPost, "/v1/tokens"},
		{http.MethodGet, "/admin/accounts"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/login"},
		{http.MethodGet, "/healthz"},
		// The five console OAuth routes, spec 0024 AC-25a. The deploy host
		// pattern gains the two discovery documents and nothing else: the
		// authorization server, the registration, the consent page and the
		// token endpoint all belong to the console.
		{http.MethodGet, "/.well-known/oauth-authorization-server"},
		{http.MethodPost, "/oauth/register"},
		{http.MethodGet, "/oauth/authorize"},
		{http.MethodPost, "/oauth/authorize"},
		{http.MethodPost, "/oauth/token"},
	} {
		if rec := h.onHost(t, tc.method, testMCPHost, tc.path); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the deploy host = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}

	// Present on the deploy host. The upload route answers 401 rather than 404,
	// which is the whole distinction: it is registered and it refused the
	// credential, rather than not being there at all.
	if rec := h.onHost(t, http.MethodPost, testMCPHost, "/v1/uploads"); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /v1/uploads on the deploy host = %d, want 401 from the registered route", rec.Code)
	}
	// The MCP stand in answers 418, so reaching it proves the registration
	// rather than proving anything about MCP.
	if rec := h.onHost(t, http.MethodPost, testMCPHost, "/mcp"); rec.Code != http.StatusTeapot {
		t.Errorf("POST /mcp on the deploy host = %d, want the registered handler", rec.Code)
	}
}

// TestARouteNobodyRegistersTwiceIsAbsentFromTheDeployHost is AC-2. The direction
// this fails in is the private one: a route added to the mux later answers on the
// default pattern and 404s on the deploy host, because registration there is opt
// in rather than something to remember to exclude.
func TestARouteNobodyRegistersTwiceIsAbsentFromTheDeployHost(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	h := newHarness(t)
	h.mux.HandleFunc("GET /v1/something-added-later", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	if rec := h.onHost(t, http.MethodGet, testMCPHost, "/v1/something-added-later"); rec.Code != http.StatusNotFound {
		t.Errorf("the new route answers %d on the deploy host, want 404: registration there must be opt in",
			rec.Code)
	}
	if rec := h.onHost(t, http.MethodGet, "deployer.example.test", "/v1/something-added-later"); rec.Code != http.StatusOK {
		t.Errorf("the new route answers %d on the default pattern, want 200", rec.Code)
	}
}

// TestTheCutoverTookTheDeployRoutesOffTheTailnet is AC-5. The deploy path used
// to answer on the default pattern as well, which is what the tailnet name
// reaches, and spec 0022 retires that half once the public one is proved. Both
// routes now live under the deploy host alone and answer a plain 404 on the
// tailnet, the same as any route nobody registered.
//
// The single use fetch is the one that must not move. It is read by a build's
// init container over cluster DNS, which knows no public name, so it stays on
// the default pattern and stays off the deploy host (AC-4). The two halves are
// asserted together on purpose: this is the commit where confusing them breaks
// every build rather than leaking a route.
func TestTheCutoverTookTheDeployRoutesOffTheTailnet(t *testing.T) {
	// covers: AC-5
	t.Parallel()
	h := newHarness(t)

	const tailnet = "deployer.example.test"

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/uploads"},
		{http.MethodPost, "/mcp"},
		{http.MethodGet, "/mcp"},
	} {
		if rec := h.onHost(t, tc.method, tailnet, tc.path); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the tailnet name = %d, want 404: the cutover took it off this pattern",
				tc.method, tc.path, rec.Code)
		}
	}

	// Still there, and it has to be: a build's init container reaches this on
	// cluster DNS and has no other way to fetch its source. A 404 here means
	// every deploy fails at the fetch.
	if rec := h.onHost(t, http.MethodGet, tailnet, "/v1/uploads/upl_nothing"); rec.Code == http.StatusNotFound {
		t.Error("GET /v1/uploads/{id} answered 404 on the default pattern: the init container can no longer fetch its source")
	}
}

// TestTheConsoleHostCarriesNoDeployRoute is AC-3. Adding the deploy host
// registered nothing on the console: a route that changes cluster state is
// absent from that hostname's mux rather than refused by a check inside it.
func TestTheConsoleHostCarriesNoDeployRoute(t *testing.T) {
	// covers: AC-3
	t.Parallel()
	h := newHarness(t)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/uploads"},
		{http.MethodGet, "/v1/uploads/upl_1"},
		{http.MethodPost, "/mcp"},
	} {
		if rec := h.onHost(t, tc.method, testConsoleHost, tc.path); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the console host = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

// TestAnOversizedBodyIsRefusedWithAClosedCode is AC-11 and AC-12. Both gates
// answer the same closed reason code and write an audit row: the declared length,
// refused before a byte is read, and the body that declared nothing.
func TestAnOversizedBodyIsRefusedWithAClosedCode(t *testing.T) {
	// covers: AC-11, AC-12
	t.Parallel()
	for _, tc := range []struct {
		name    string
		declare bool
	}{
		{"a declared length over the ceiling", true},
		{"a body that declared nothing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			req := post(t, goodToken, oversized(t))
			if !tc.declare {
				// A body that lied about its size, which is what makes the
				// second gate the socket rather than the header.
				req.ContentLength = -1
			}

			rec := h.do(t, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body)
			}
			if got := errorCode(t, rec); got != string(domain.ReasonUploadTooLarge) {
				t.Errorf("error = %q, want the closed code %q", got, domain.ReasonUploadTooLarge)
			}
			if h.auditRows(t, auth.ActionUpload, "denied") != 1 {
				t.Error("want one denied audit row for the refused upload")
			}
		})
	}
}

// TestANonGzipBodyIsRefusedWithAClosedCode is AC-12 and AC-19. The refusal a
// caller sees is a code from the closed set, never a wrapped error string.
func TestANonGzipBodyIsRefusedWithAClosedCode(t *testing.T) {
	// covers: AC-12, AC-19
	t.Parallel()
	h := newHarness(t)

	rec := h.do(t, post(t, goodToken, []byte("this was never a gzip stream")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	if got := errorCode(t, rec); got != string(domain.ReasonUploadNotGzip) {
		t.Errorf("error = %q, want the closed code %q", got, domain.ReasonUploadNotGzip)
	}
}

// TestTheUnclaimedUploadCapRefusesTheNextOne is AC-17. An account holds at most
// its ceiling of uploads no deploy has claimed, nothing lands on the volume when
// one is refused, and the refusal is audited.
func TestTheUnclaimedUploadCapRefusesTheNextOne(t *testing.T) {
	// covers: AC-17
	t.Parallel()
	h := newHarness(t)

	for i := range maxTestUnclaimed {
		if rec := h.do(t, post(t, goodToken, tarball(t, "source"))); rec.Code != http.StatusCreated {
			t.Fatalf("upload %d = %d, want 201: %s", i+1, rec.Code, rec.Body)
		}
	}
	before := h.filesOnVolume(t)

	rec := h.do(t, post(t, goodToken, tarball(t, "one too many")))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body)
	}
	if got := errorCode(t, rec); got != string(domain.ReasonUploadLimitReached) {
		t.Errorf("error = %q, want the closed code %q", got, domain.ReasonUploadLimitReached)
	}
	// Nothing reached the volume. The refusal happens before the write, so a
	// caller at its ceiling costs the platform no disk at all.
	if after := h.filesOnVolume(t); after != before {
		t.Errorf("the volume holds %d files, want the %d it held before the refusal", after, before)
	}
	if h.auditRows(t, auth.ActionUpload, "denied") != 1 {
		t.Error("want one denied audit row for the refused upload")
	}
}

// TestTheSweepTakesAnExpiredUploadAndLeavesAReferencedOne is AC-18. The file and
// the row go together, and an upload a deployment still names is left alone so
// deploy history stays intact.
func TestTheSweepTakesAnExpiredUploadAndLeavesAReferencedOne(t *testing.T) {
	// covers: AC-18
	t.Parallel()
	h := newHarness(t)

	rec := h.do(t, post(t, goodToken, tarball(t, "source")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("uploading = %d, want 201: %s", rec.Code, rec.Body)
	}
	id := decoded(t, rec)["upload_id"]

	// Expired by hand. The window is an hour, which is not a thing a test waits
	// out, and the sweep reads the column rather than the clock it was written
	// by.
	h.expireUpload(t, id)

	swept, err := h.uploads.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept %d uploads, want 1", swept)
	}
	if n := h.filesOnVolume(t); n != 0 {
		t.Errorf("the volume holds %d files, want none: the file and the row go together", n)
	}
	if h.uploadRows(t) != 0 {
		t.Error("the row survived its file, which is a row nothing can ever serve")
	}

	// A second upload, expired but named by a deployment, is left alone.
	rec = h.do(t, post(t, goodToken, tarball(t, "referenced source")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("uploading = %d, want 201: %s", rec.Code, rec.Body)
	}
	referenced := decoded(t, rec)["upload_id"]
	h.referenceUpload(t, referenced)
	h.expireUpload(t, referenced)

	swept, err = h.uploads.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweeping again: %v", err)
	}
	if swept != 0 {
		t.Errorf("swept %d uploads, want 0: a deployment still names that row", swept)
	}
	if h.uploadRows(t) != 1 {
		t.Error("the referenced row was deleted, so a release lost the source it was built from")
	}
}

// errorCode reads the closed reason code out of a refusal body.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decoded(t, rec)["error"]
}

// decoded reads a JSON object response.
func decoded(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return body
}
