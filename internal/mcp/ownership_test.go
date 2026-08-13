// Package mcp_test drives the real tool surface over the real transport, against
// a real SQLite file and two real registered accounts. It is an external test
// package because it imports internal/store, which imports internal/mcp: the
// ownership boundary can only be proved from outside.
package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/mcp"
	"github.com/toyinogun/deployer/internal/store"
	"github.com/toyinogun/deployer/internal/uploads"
)

// person is one registered account plus the API token their agent carries.
type person struct {
	account store.Account
	token   string
}

// ownershipHarness is the tool surface over a real store holding two people.
type ownershipHarness struct {
	server *httptest.Server
	store  *store.Store
	a, b   person
}

func newOwnershipHarness(t *testing.T) *ownershipHarness {
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
	authenticator := auth.NewAuthenticator(as, as).WithSessions(as, identity.SessionLifetime)
	uploadSvc := uploads.NewService(store.ForUploads(st), filepath.Join(dir, "uploads"), 4096, nil)

	tools := mcp.New(authenticator, as, store.ForMCPApps(st), store.ForMCPDeployments(st),
		forTool{svc: uploadSvc}, nil, acceptingCluster{}, mcp.Options{
			PublicURL: "https://deploy.example.org",
			AppDomain: "apps.example.org",
		})
	srv := httptest.NewServer(tools.Handler())
	t.Cleanup(srv.Close)

	h := &ownershipHarness{server: srv, store: st}
	h.a = h.enroll(t, "a@example.com")
	h.b = h.enroll(t, "b@example.com")
	return h
}

// enroll registers a verified person and mints them an API token, which is the
// state every real caller reaches before it deploys anything.
func (h *ownershipHarness) enroll(t *testing.T, email string) person {
	t.Helper()
	ctx := t.Context()
	acc, err := h.store.CreateIdentityAccount(ctx, store.NewIdentityAccount{
		Email: email, PasswordHash: "argon2id$fake", DisplayName: email,
	})
	if err != nil {
		t.Fatalf("registering %s: %v", email, err)
	}
	if err := h.store.MarkEmailVerified(ctx, acc.ID); err != nil {
		t.Fatalf("verifying %s: %v", email, err)
	}
	raw, err := identity.NewAPIToken()
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if _, err := h.store.CreateAPIToken(ctx, store.NewToken{
		AccountID: acc.ID, Name: "agent",
		TokenHash: identity.HashSecret(raw), Prefix: identity.TokenPrefix(raw),
	}); err != nil {
		t.Fatalf("storing the token: %v", err)
	}
	return person{account: acc, token: raw}
}

// call runs one tool as the given person, returning the structured result, the
// text the tool answered with, and whether it refused. A refusal carries its
// closed reason code in the text rather than in the structured content, which is
// empty when nothing succeeded.
func (h *ownershipHarness) call(t *testing.T, who person, tool string, args map[string]any) (map[string]any, string, bool) {
	t.Helper()
	ctx := t.Context()

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             h.server.URL,
		HTTPClient:           &http.Client{Transport: bearer{token: who.token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connecting as %s: %v", who.account.ID, err)
	}
	defer func() { _ = session.Close() }()

	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", tool, err)
	}
	out := map[string]any{}
	if res.StructuredContent != nil {
		encoded, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("re-encoding the result: %v", err)
		}
		if err := json.Unmarshal(encoded, &out); err != nil {
			t.Fatalf("reading the result: %v", err)
		}
	}
	var said strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			said.WriteString(text.Text)
		}
	}
	return out, said.String(), res.IsError
}

// bearer attaches one person's API token to every request the client makes.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(clone)
}

// acceptingCluster stands in for the real namespace delete. The fake clientset
// is exercised in internal/kube; what these tests need is a cluster that says
// yes, so a delete_app reaches its audit row and its response.
type acceptingCluster struct{}

func (acceptingCluster) DeleteNamespace(context.Context, string) error { return nil }

// forTool is the upload lookup the tool surface needs.
type forTool struct{ svc *uploads.Service }

func (f forTool) Get(ctx context.Context, id string) (mcp.Upload, error) {
	up, err := f.svc.Get(ctx, id)
	if err != nil {
		return mcp.Upload{}, err
	}
	return mcp.Upload{
		ID: up.ID, AccountID: up.AccountID, ExpiresAt: up.ExpiresAt, Redeemed: up.Redeemed,
	}, nil
}

// upload puts a tarball on the platform as one person, so a deploy has a source.
func (h *ownershipHarness) upload(t *testing.T, who person) string {
	t.Helper()
	fresh, err := identity.NewSecret()
	if err != nil {
		t.Fatalf("drawing a fetch token: %v", err)
	}
	up, createErr := h.store.CreateUpload(t.Context(), store.NewUpload{
		AccountID:      who.account.ID,
		Path:           filepath.Join(t.TempDir(), "source.tgz"),
		SizeBytes:      10,
		SHA256:         "sha-" + who.account.ID,
		FetchTokenHash: identity.HashSecret(fresh),
		ExpiresAt:      ids.Stamp(ids.SystemClock{}.Now().Add(time.Hour)),
	})
	if createErr != nil {
		t.Fatalf("creating an upload: %v", createErr)
	}
	return up.ID
}

// TestOneAccountCannotReachAnothersApp is AC-17 and AC-21 together: with two real
// registered accounts, the second cannot deploy to, read the status of, read logs
// from, or discover the existence of the first's app. Each refusal is the closed
// reason code the platform already uses, and each writes an audit row.
func TestOneAccountCannotReachAnothersApp(t *testing.T) {
	h := newOwnershipHarness(t)

	// A deploys, which creates the app under A's ownership.
	out, _, isErr := h.call(t, h.a, "deploy_app", map[string]any{
		"name": "checkout", "upload_id": h.upload(t, h.a),
	})
	if isErr {
		t.Fatalf("A's own deploy was refused: %v", out)
	}
	deploymentID, _ := out["deployment_id"].(string)
	if deploymentID == "" {
		t.Fatalf("no deployment id came back: %v", out)
	}

	before := auditCount(t, h.store, h.b.account.ID)

	// B deploying to the same name gets its own app, not A's, which is AC-18: two
	// accounts may each own an app called the same thing.
	bOut, _, bErr := h.call(t, h.b, "deploy_app", map[string]any{
		"name": "checkout", "upload_id": h.upload(t, h.b),
	})
	if bErr {
		t.Fatalf("B's own deploy was refused: %v", bOut)
	}
	if bOut["url"] == out["url"] {
		t.Errorf("both accounts got the same hostname %v, so a slug is not unique per app", out["url"])
	}

	// B reading A's deployment is refused, and told nothing about it existing.
	statusOut, statusSaid, statusErr := h.call(t, h.b, "deployment_status", map[string]any{
		"deployment_id": deploymentID,
	})
	if !statusErr {
		t.Errorf("B read A's deployment status: %v", statusOut)
	}
	if !strings.Contains(statusSaid, string(domain.ReasonDeploymentUnknown)) {
		t.Errorf("status refusal is %q, want %s", statusSaid, domain.ReasonDeploymentUnknown)
	}

	// B reading A's app by name finds nothing, because names are scoped per
	// account: B's own "checkout" is a different app.
	logsOut, logsSaid, logsErr := h.call(t, h.b, "get_logs", map[string]any{"name": "not-b-s-app"})
	if !logsErr {
		t.Errorf("B read logs for an app it does not own: %v", logsOut)
	}
	if !strings.Contains(logsSaid, string(domain.ReasonAppUnknown)) {
		t.Errorf("logs refusal is %q, want %s", logsSaid, domain.ReasonAppUnknown)
	}

	// B deploying with A's upload is refused: an upload is owned too.
	uploadOut, uploadSaid, uploadErr := h.call(t, h.b, "deploy_app", map[string]any{
		"name": "borrowed", "upload_id": h.upload(t, h.a),
	})
	if !uploadErr {
		t.Errorf("B deployed A's upload: %v", uploadOut)
	}
	if !strings.Contains(uploadSaid, string(domain.ReasonUploadInvalid)) {
		t.Errorf("upload refusal is %q, want %s", uploadSaid, domain.ReasonUploadInvalid)
	}

	if after := auditCount(t, h.store, h.b.account.ID); after <= before {
		t.Error("B's refusals were not recorded in the audit log")
	}
}

// TestAdminHasNoOverrideOnApps is AC-21: admin adds visibility over accounts and
// nothing over apps, so an admin's own token reaches its own apps and no others.
func TestAdminHasNoOverrideOnApps(t *testing.T) {
	h := newOwnershipHarness(t)
	// A is the first registered account, so A is the admin.
	if h.a.account.IsAdmin != 1 {
		t.Fatalf("the first account is not admin, so this test is not testing an admin")
	}

	out, _, isErr := h.call(t, h.b, "deploy_app", map[string]any{
		"name": "private", "upload_id": h.upload(t, h.b),
	})
	if isErr {
		t.Fatalf("B's own deploy was refused: %v", out)
	}
	deploymentID, _ := out["deployment_id"].(string)

	adminOut, adminSaid, adminErr := h.call(t, h.a, "deployment_status", map[string]any{
		"deployment_id": deploymentID,
	})
	if !adminErr {
		t.Errorf("the admin read another account's deployment: %v", adminOut)
	}
	if !strings.Contains(adminSaid, string(domain.ReasonDeploymentUnknown)) {
		t.Errorf("the admin's refusal is %q, want %s", adminSaid, domain.ReasonDeploymentUnknown)
	}
}

// auditCount is how many rows one account has in the audit log.
func auditCount(t *testing.T, s *store.Store, accountID string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_log WHERE account_id = ?`, accountID).Scan(&n); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	return n
}
