package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
)

// testMCPURL is the deploy path's public address in these tests.
const testMCPURL = "https://mcp.apps.example.test"

// challenged wraps the middleware with an MCPURL set, which is what the
// WWW-Authenticate header is composed from. A nil limiter takes the roomy test
// one, so only the test that wants a 429 has to build a tight bucket.
func challenged(store auth.Store, auditor auth.Auditor, limiter *identity.Limiter) http.Handler {
	if limiter == nil {
		limiter = testLimiter()
	}
	s := &Server{
		auth:    auth.NewAuthenticator(store, nil),
		auditor: auditor,
		limiter: limiter,
		opts:    Options{MCPURL: testMCPURL},
	}
	return s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

// AC-2. A client that holds no token is told where to go and get one. Without
// this header the deploy path is silent about how to connect, which is the
// difference between a client that can add this platform and one that cannot.
func TestAnUnauthenticatedCallIsToldWhereToSignIn(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	challenged(tokenStore{liveHash: "nothing matches this"}, &countingAuditor{}, nil).
		ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	got := rec.Header().Get("WWW-Authenticate")
	want := `Bearer resource_metadata="` + testMCPURL +
		`/.well-known/oauth-protected-resource/mcp", scope="deploy"`
	if got != want {
		t.Errorf("the challenge is %q,\nwant %q", got, want)
	}
	// The body and its shape are unchanged, because an existing client reads it.
	if !strings.Contains(rec.Body.String(), `"error":"unauthorized"`) {
		t.Errorf("the 401 body changed: %s", rec.Body)
	}
}

// AC-2a. The refusals that are not about the credential gain no header. A
// client reading one of these as a sign in prompt would retry the whole OAuth
// flow against a rate limit, which is the opposite of backing off.
func TestARefusalThatIsNotAboutTheCredentialCarriesNoChallenge(t *testing.T) {
	t.Parallel()
	// The limiter's 429: one token in the bucket, so the second call is refused
	// for the rate rather than for the credential.
	tight := identity.NewLimiter(ids.SystemClock{}, identity.Settings{
		BucketCapacity:        1,
		BucketRefill:          time.Hour,
		FailuresBeforeLockout: 1 << 20,
		LockoutBase:           time.Second,
		LockoutCeiling:        time.Second,
	})
	handler := challenged(tokenStore{liveHash: "nothing matches this"}, &countingAuditor{}, tight)
	var last *httptest.ResponseRecorder
	for range 5 {
		last = httptest.NewRecorder()
		handler.ServeHTTP(last, httptest.NewRequest(http.MethodPost, "/mcp", nil))
		if last.Code == http.StatusTooManyRequests {
			break
		}
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("the bucket never ran dry: last was %d", last.Code)
	}
	if got := last.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("the 429 carries a challenge: %q", got)
	}
}

// A call that authenticates carries no challenge either. Claude ignores the
// header on anything but a 401, and advertising a sign in to a caller already
// signed in is noise at best.
func TestAnAuthenticatedCallCarriesNoChallenge(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	challenged(liveStore(), &countingAuditor{}, nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("a successful call carries a challenge: %q", got)
	}
}
