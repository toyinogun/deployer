package mcp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/httpapi"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/mcp"
	"github.com/toyinogun/deployer/internal/store"
	"github.com/toyinogun/deployer/internal/uploads"
)

// deployPathHarness is the whole deploy path as the cluster serves it: the upload
// endpoint and the MCP endpoint on one mux, behind one authenticator and one
// limiter.
//
// Both routes have to be here together. AC-16 is that the rule lives in the
// authenticator rather than in a handler, and the only way to tell that apart
// from a copy in one handler is to spend the penalty on one route and see the
// other honour it.
// deployPathHost is the hostname both deploy routes answer on, and since the
// cutover the only one.
const deployPathHost = "mcp.apps.example.org"

type deployPathHarness struct {
	server *httptest.Server
	clock  *ids.FixedClock
}

func newDeployPathHarness(t *testing.T) *deployPathHarness {
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

	clock := &ids.FixedClock{T: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	// The real lockout numbers, with a bucket wide enough that the token bucket
	// never fires first. What is under test here is the penalty a run of bad
	// credentials earns, not the call rate.
	settings := identity.DeployPathSettings()
	settings.BucketCapacity = 1 << 20
	limiter := identity.NewLimiter(clock, settings)
	authenticator := auth.NewAuthenticator(as, as).WithLockout(limiter)

	uploadSvc := uploads.NewService(store.ForUploads(st), filepath.Join(dir, "uploads"), 4096, 3, clock)
	tools := mcp.New(authenticator, as, store.ForMCPApps(st), store.ForMCPDeployments(st),
		forTool{svc: uploadSvc}, nil, acceptingCluster{}, mcp.Options{
			MCPURL:            "https://" + deployPathHost,
			AppDomain:         "apps.example.org",
			MaxAppsPerAccount: 10,
			Limiter:           limiter,
		})

	mux := http.NewServeMux()
	httpapi.New(authenticator, as, uploadSvc, limiter, httpapi.Options{
		MaxBytes: 4096,
		// Both routes live under this hostname and no other since spec 0022's
		// cutover, so a harness without it registers nothing and every call
		// here answers 404 instead of exercising the lockout (AC-5).
		MCPHost: deployPathHost,
	}).Register(mux, tools.Handler())

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &deployPathHarness{server: server, clock: clock}
}

// call presents a bearer token to one of the two routes and returns the status
// and the decoded body.
func (h *deployPathHarness) call(t *testing.T, path, token string) (int, map[string]string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+path, strings.NewReader(""))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	// The deploy host is the only pattern these routes answer on, so the Host
	// carries it rather than the test server's address.
	req.Host = deployPathHost
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("calling %s: %v", path, err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			t.Errorf("closing the response body: %v", err)
		}
	}()
	var body map[string]string
	// A body that is not the refusal shape is fine here: only the refusals
	// carry one, and the status is what the non refusal cases are read on.
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// TestABadTokenRunLocksBothDeployRoutes is AC-16 and AC-19.
//
// The penalty is spent entirely on the upload route and then honoured by the MCP
// endpoint, which is what proves the rule is inside auth.Authenticator. A copy in
// one handler would pass every assertion up to the last one and fail there.
func TestABadTokenRunLocksBothDeployRoutes(t *testing.T) {
	// covers: AC-16, AC-19
	t.Parallel()
	h := newDeployPathHarness(t)

	// Below the free allowance, a bad token is only ever a bad token.
	for i := range identity.DeployPathSettings().FailuresBeforeLockout - 1 {
		if status, _ := h.call(t, "/v1/uploads", "dpl_never_minted"); status != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401 while the free attempts last", i+1, status)
		}
	}

	// The one that spends the last free attempt earns the window.
	if status, _ := h.call(t, "/v1/uploads", "dpl_never_minted"); status != http.StatusUnauthorized {
		t.Fatalf("the last free attempt = %d, want 401", status)
	}

	status, body := h.call(t, "/v1/uploads", "dpl_never_minted")
	if status != http.StatusTooManyRequests {
		t.Fatalf("the upload route = %d after the run, want 429", status)
	}
	if body["error"] != string(domain.ReasonTooManyAttempts) {
		t.Errorf("error = %q, want the closed code %q", body["error"], domain.ReasonTooManyAttempts)
	}

	// The MCP endpoint spent none of that run and honours all of it, because
	// neither handler decides this.
	status, body = h.call(t, "/mcp", "dpl_never_minted")
	if status != http.StatusTooManyRequests {
		t.Fatalf("the MCP endpoint = %d, want 429: the penalty belongs to the authenticator, not to a handler",
			status)
	}
	if body["error"] != string(domain.ReasonTooManyAttempts) {
		t.Errorf("error = %q, want the closed code %q", body["error"], domain.ReasonTooManyAttempts)
	}
	// Nothing internal crosses the boundary, and nothing echoes the token.
	if strings.Contains(body["message"], "dpl_never_minted") {
		t.Error("the refusal echoes the presented token")
	}
}

// TestThePenaltyEndsWhenItsWindowDoes is AC-16. The lockout is a delay rather
// than a ban, so an address is never locked out for good.
func TestThePenaltyEndsWhenItsWindowDoes(t *testing.T) {
	// covers: AC-16
	t.Parallel()
	h := newDeployPathHarness(t)

	for range identity.DeployPathSettings().FailuresBeforeLockout {
		h.call(t, "/v1/uploads", "dpl_never_minted")
	}
	if status, _ := h.call(t, "/mcp", "dpl_never_minted"); status != http.StatusTooManyRequests {
		t.Fatalf("the run earned no penalty")
	}

	h.clock.Advance(identity.DeployPathSettings().LockoutBase + time.Second)

	// Back to an ordinary bad credential: refused, but on its own merits.
	if status, _ := h.call(t, "/mcp", "dpl_never_minted"); status != http.StatusUnauthorized {
		t.Errorf("after the window = %d, want 401: the penalty is a delay, not a ban", status)
	}
}

// TestTheDeployPathRateLimitIsNotTheSignInOne is AC-15. The two limiters are
// separate instances with separate numbers, so a burst on the deploy path cannot
// spend a person's sign in budget or lock them out of the console.
func TestTheDeployPathRateLimitIsNotTheSignInOne(t *testing.T) {
	// covers: AC-15
	t.Parallel()
	deploy, signIn := identity.DeployPathSettings(), identity.SignInSettings()

	if deploy.BucketCapacity <= signIn.BucketCapacity {
		t.Errorf("the deploy path bucket holds %v and the sign in one holds %v: an agent polling a build "+
			"through the deploy path needs the wider one", deploy.BucketCapacity, signIn.BucketCapacity)
	}
	if deploy.BucketRefill >= signIn.BucketRefill {
		t.Errorf("the deploy path refills every %v and sign in every %v: the deploy path needs the faster one",
			deploy.BucketRefill, signIn.BucketRefill)
	}
	// The sign in numbers are exactly what they were before spec 0022 moved
	// them off package constants, so that refactor changed no sign in behaviour.
	if signIn != (identity.Settings{
		BucketCapacity:        10,
		BucketRefill:          6 * time.Second,
		FailuresBeforeLockout: 5,
		LockoutBase:           30 * time.Second,
		LockoutCeiling:        15 * time.Minute,
	}) {
		t.Errorf("the sign in settings are %+v, which is not what they were before they moved", signIn)
	}
}
