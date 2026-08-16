package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
)

// Joining, spec 0023. The page that hands a person a finished configuration
// block with a real token in it, and the one time redirect that takes them there
// after they verify.

// tokenInBlock pulls the credential back out of a rendered block, which is the
// only way a test can see it: it is in that one response body and nowhere else.
var tokenInBlock = regexp.MustCompile(`Bearer (dpl_[A-Za-z0-9_-]+)`)

// mintedToken drives one mint and hands back the response and the raw value it
// carried.
func (h *harness) mintedToken(t *testing.T, cookie *http.Cookie, client string) (string, string) {
	t.Helper()
	rec := h.postRaw(t, "/connect", url.Values{
		csrfField: {h.csrfFor(t, cookie)}, "client": {client},
	}, nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("minting from /connect: got %d, want 200: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	match := tokenInBlock.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("no token in any block after a mint: %s", body)
	}
	return match[1], body
}

// connectedAt reads the stamp straight off the row, which is the only place it
// exists: nothing renders it and no service hands it back.
func (h *harness) connectedAt(t *testing.T, accountID string) string {
	t.Helper()
	acc, err := h.store.GetAccount(t.Context(), accountID)
	if err != nil {
		t.Fatalf("reading account %s: %v", accountID, err)
	}
	if acc.ConnectedAt == nil {
		return ""
	}
	return *acc.ConnectedAt
}

// TestAVerifiedPersonIsSentToConnectExactlyOnce is the whole point of the
// feature's entry: the first plain sign in after verifying lands on the page, and
// once the page has been served it never happens again.
// covers: AC-3, AC-3a, AC-4
func TestAVerifiedPersonIsSentToConnectExactlyOnce(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "joiner@example.test")

	first := h.post(t, "/login", url.Values{
		"email": {"joiner@example.test"}, "password": {testPassword},
	}, nil, nil)
	if first.Code != http.StatusSeeOther || first.Header().Get("Location") != "/connect" {
		t.Fatalf("a verified, unconnected sign in went to %q with %d, want /connect",
			first.Header().Get("Location"), first.Code)
	}

	if rec := h.get(t, "/connect", cookie); rec.Code != http.StatusOK {
		t.Fatalf("GET /connect: got %d, want 200: %s", rec.Code, rec.Body)
	}

	again := h.post(t, "/login", url.Values{
		"email": {"joiner@example.test"}, "password": {testPassword},
	}, nil, nil)
	if again.Header().Get("Location") != "/apps" {
		t.Errorf("a connected sign in went to %q, want /apps", again.Header().Get("Location"))
	}
}

// TestTheStampIsNotMovedBySecondVisit is the other half of AC-4: the page is
// useful to come back to, and coming back must not rewrite when the person
// joined.
// covers: AC-4
func TestTheStampIsNotMovedBySecondVisit(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "returning@example.test")
	account := h.accountID(t, cookie)

	h.get(t, "/connect", cookie)
	first := h.connectedAt(t, account)
	if first == "" {
		t.Fatal("the first GET /connect left no stamp")
	}

	h.clock.T = h.clock.T.Add(48 * time.Hour)
	h.get(t, "/connect", cookie)
	if second := h.connectedAt(t, account); second != first {
		t.Errorf("a second visit moved the stamp from %q to %q", first, second)
	}

	// The route skips the write when the session's own row already carries the
	// stamp, so the assertion above passes even if the statement underneath
	// stopped being conditional. This is the half that actually holds: the
	// statement itself, called again two days later, still moves nothing.
	did, err := h.store.MarkAccountConnected(t.Context(), account)
	if err != nil {
		t.Fatalf("stamping an already stamped account: %v", err)
	}
	if did {
		t.Error("the stamp statement wrote over an existing stamp")
	}
	if again := h.connectedAt(t, account); again != first {
		t.Errorf("calling the stamp again moved it from %q to %q", first, again)
	}
}

// TestTheStampCannotRace is AC-4a. The statement is conditional rather than a
// read then a write, so however many first visits arrive at once, exactly one of
// them stamps.
// covers: AC-4a
func TestTheStampCannotRace(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "racer@example.test")
	account := h.accountID(t, cookie)

	// The statement itself, driven concurrently. Only the store can say which
	// call was the one that stamped, and exactly one of them must be.
	var stamped atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			did, err := h.store.MarkAccountConnected(t.Context(), account)
			if err != nil {
				t.Errorf("stamping concurrently: %v", err)
				return
			}
			if did {
				stamped.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := stamped.Load(); got != 1 {
		t.Errorf("%d of 8 concurrent calls stamped the account, want exactly 1", got)
	}

	// And the route over it: two first visits at once, neither failing and the
	// account stamped once at the end.
	other := h.signIn(t, "racer2@example.test")
	otherID := h.accountID(t, other)
	var pages sync.WaitGroup
	for range 2 {
		pages.Add(1)
		go func() {
			defer pages.Done()
			if rec := h.get(t, "/connect", other); rec.Code != http.StatusOK {
				t.Errorf("a concurrent GET /connect: got %d", rec.Code)
			}
		}()
	}
	pages.Wait()
	if h.connectedAt(t, otherID) == "" {
		t.Error("two concurrent first visits left no stamp")
	}
}

// TestAnAccountWithNoVerifiedAddressIsNeverRedirected is AC-5. The bootstrap
// account holds no address, so it is never verified and never satisfies the
// condition, quite apart from being refused by Login before it gets here.
// covers: AC-5
func TestAnAccountWithNoVerifiedAddressIsNeverRedirected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		account identity.Account
		want    string
	}{
		{"the bootstrap account holds no address", identity.Account{}, "/apps"},
		{"an unverified account", identity.Account{Email: "a@example.test"}, "/apps"},
		{"verified and never connected", identity.Account{Email: "a@example.test", Verified: true}, "/connect"},
		{"verified and already connected",
			identity.Account{Email: "a@example.test", Verified: true, Connected: true}, "/apps"},
	} {
		if got := afterSignIn("", tc.account); got != tc.want {
			t.Errorf("%s: afterSignIn = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestADeepLinkOutranksTheConnectRedirect is the rest of AC-3. A next only
// exists because the session gate put it there, so the person was already trying
// to reach a page and dropping that intent is the worse failure. They stay
// unstamped, so the next plain sign in still takes them to /connect.
// covers: AC-3
func TestADeepLinkOutranksTheConnectRedirect(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t, "deep@example.test")

	deep := h.post(t, "/login", url.Values{
		"email": {"deep@example.test"}, "password": {testPassword}, "next": {"/tokens"},
	}, nil, nil)
	if got := deep.Header().Get("Location"); got != "/tokens" {
		t.Fatalf("a sign in carrying next=/tokens went to %q", got)
	}

	plain := h.post(t, "/login", url.Values{
		"email": {"deep@example.test"}, "password": {testPassword},
	}, nil, nil)
	if got := plain.Header().Get("Location"); got != "/connect" {
		t.Errorf("the plain sign in after a deep link went to %q, want /connect", got)
	}
}

// TestThePageRendersFourTabsWithClaudeCodeFirst is the shape of the page: four
// named clients in a fixed order, one selected on arrival, and a noscript region
// so a browser with the script off still gets all four.
// covers: AC-7, AC-8, AC-13, AC-14
func TestThePageRendersFourTabsWithClaudeCodeFirst(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "tabs@example.test")
	body := h.get(t, "/connect", cookie).Body.String()

	want := []string{"claude-code", "codex", "gemini-cli", "mcp-json"}
	at := -1
	for _, key := range want {
		i := strings.Index(body, `data-connect-tab="`+key+`"`)
		if i < 0 {
			t.Fatalf("no tab for %s", key)
		}
		if i < at {
			t.Errorf("the %s tab is rendered out of order", key)
		}
		at = i
		if !strings.Contains(body, `data-copy="block-`+key+`"`) {
			t.Errorf("the %s block has no copy control", key)
		}
	}
	if !strings.Contains(body, `id="tab-claude-code" data-connect-tab="claude-code"`) ||
		!strings.Contains(body, `aria-controls="panel-claude-code" aria-selected="true"`) {
		t.Error("Claude Code is not the tab selected on arrival")
	}
	if !strings.Contains(body, "claude mcp add --transport http deployer ") {
		t.Error("the Claude Code tab is not a command line")
	}
	if !strings.Contains(body, "<noscript>") || !strings.Contains(body, ".connect-panel[hidden]") {
		t.Error("no noscript region, so the page is one block with the script off")
	}
}

// TestEveryBlockCarriesTheConfiguredEndpoint is AC-9. The address is derived from
// the configured deploy host rather than written into any block, so a hostname
// change moves one value and all four follow. Driven as a configuration swap in
// process, the same shape as the MCP tool description's own pinning test.
// covers: AC-9, AC-11
func TestEveryBlockCarriesTheConfiguredEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "endpoint@example.test")

	if got := strings.Count(h.get(t, "/connect", cookie).Body.String(), testMCPURL+"/mcp"); got != 4 {
		t.Errorf("the configured endpoint appears in %d blocks, want 4", got)
	}

	const moved = "https://elsewhere.example.test"
	opts := h.srv.opts
	opts.MCPURL = moved
	fresh := New(h.srv.svc, h.srv.auth, h.audit, h.data, nil,
		h.srv.suspension, h.srv.backups, h.srv.backupRuns, opts)
	mux := http.NewServeMux()
	fresh.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/connect", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()

	if got := strings.Count(body, moved+"/mcp"); got != 4 {
		t.Errorf("after the configuration moved, the new endpoint appears in %d blocks, want 4", got)
	}
	if strings.Contains(body, testMCPURL) {
		t.Error("a block still carries the old endpoint, so a hostname is written in somewhere")
	}
}

// TestEveryBlockShowsAPlaceholderUntilOneIsMinted is AC-12. No token value, past
// or present, appears on a visit that did not mint, because no path holds one to
// render.
// covers: AC-10, AC-12
func TestEveryBlockShowsAPlaceholderUntilOneIsMinted(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "placeholder@example.test")

	before := h.get(t, "/connect", cookie).Body.String()
	if got := strings.Count(before, tokenPlaceholder); got != 4 {
		t.Errorf("the placeholder appears %d times before any mint, want 4", got)
	}
	if got := strings.Count(before, "Bearer "); got != 4 {
		t.Errorf("the bearer credential appears in %d blocks, want 4", got)
	}

	raw, _ := h.mintedToken(t, cookie, "claude-code")

	after := h.get(t, "/connect", cookie).Body.String()
	if strings.Contains(after, raw) {
		t.Error("a later visit re renders a token that was already shown once")
	}
	if got := strings.Count(after, tokenPlaceholder); got != 4 {
		t.Errorf("the placeholder appears %d times on a later visit, want 4", got)
	}
}

// TestMintingPutsTheRawTokenInEveryBlock is AC-15 acted out, plus the audit row
// the mint writes. The value lives in this one response body: there is no
// redirect, because a redirect would have to carry it in a URL.
// covers: AC-15, AC-19
func TestMintingPutsTheRawTokenInEveryBlock(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "minter@example.test")
	account := h.accountID(t, cookie)

	raw, body := h.mintedToken(t, cookie, "claude-code")
	if got := strings.Count(body, raw); got != 4 {
		t.Errorf("the minted token appears %d times, want once per block", got)
	}

	row, ok := h.audit.last(auth.ActionTokenMint)
	if !ok {
		t.Fatal("minting from /connect wrote no audit row")
	}
	if row.AccountID != account || row.TargetType != "api_token" || row.TargetID == "" || !row.Allowed {
		t.Errorf("the audit row does not describe the mint: %+v", row)
	}
	if row.ClientAddress == "" {
		t.Error("the audit row carries no client address")
	}
}

// TestASecondMachineOnTheSameDayStillMints is AC-16a. The dated default name is
// already live after the first mint, and identity refuses a name an account
// holds, so without the ordinal the second machine is a refusal rather than a
// token.
// covers: AC-16, AC-16a, AC-23
func TestASecondMachineOnTheSameDayStillMints(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "second@example.test")
	account := h.accountID(t, cookie)

	first, _ := h.mintedToken(t, cookie, "claude-code")
	second, _ := h.mintedToken(t, cookie, "claude-code")
	if first == second {
		t.Fatal("two mints handed back the same token")
	}

	tokens, err := h.srv.svc.ListTokens(t.Context(), account)
	if err != nil {
		t.Fatalf("listing tokens: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("got %d live tokens after two mints, want 2", len(tokens))
	}
	dated := "Claude Code " + h.clock.T.UTC().Format("2006-01-02")
	names := map[string]bool{tokens[0].Name: true, tokens[1].Name: true}
	if !names[dated] || !names[dated+" (2)"] {
		t.Errorf("the two tokens are named %v, want %q and its ordinal", names, dated)
	}
}

// TestAnUnknownClientFieldFallsBackToTheGenericTab is AC-17. A stale or tampered
// field costs a tab selection, never the mint.
// covers: AC-17
func TestAnUnknownClientFieldFallsBackToTheGenericTab(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "stale@example.test")

	raw, body := h.mintedToken(t, cookie, "a-client-that-does-not-exist")
	if got := strings.Count(body, raw); got != 4 {
		t.Errorf("an unknown client field cost the mint: the token appears %d times", got)
	}
	if !strings.Contains(body, `aria-controls="panel-mcp-json" aria-selected="true"`) {
		t.Error("an unknown client field did not fall back to the generic tab")
	}
	if !strings.Contains(body, genericClient.Label+" ") {
		t.Error("the token was not named for the tab it fell back to")
	}
}

// TestAMintWithoutTheSessionTokenIsRefused is AC-18: the same mechanism /tokens
// takes, not a second one.
// covers: AC-18
func TestAMintWithoutTheSessionTokenIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "csrf@example.test")

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"no token at all", ""},
		{"somebody else's token", "0123456789abcdef"},
	} {
		rec := h.postRaw(t, "/connect", url.Values{csrfField: {tc.token}}, nil, cookie)
		if rec.Code != http.StatusForbidden {
			t.Errorf("a mint with %s: got %d, want 403", tc.name, rec.Code)
		}
		if tokenInBlock.MatchString(rec.Body.String()) {
			t.Errorf("a mint refused for %s still rendered a token", tc.name)
		}
	}
}

// TestASignedOutVisitorTakesTheSessionGate is AC-1's second sentence. Being
// signed out is not a refusal, it is not having answered yet.
// covers: AC-1
func TestASignedOutVisitorTakesTheSessionGate(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.get(t, "/connect", nil)
	want := "/login?next=" + url.QueryEscape("/connect")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
		t.Errorf("GET /connect signed out: got %d to %q, want 303 to %q",
			rec.Code, rec.Header().Get("Location"), want)
	}
}

// Both routes answering on the console host is AC-1 and AC-2, and it is held by
// the enumerated list in consoleedge_test.go rather than by a test here: that
// list drives all three hosts and is the one place a route missing a second
// registration is caught.

// TestATokenMintedHereIsAnOrdinaryToken is AC-20 and AC-21. There is one token
// table, one list, one revoke path and one authenticator, and this page adds no
// second credential model to any of them.
// covers: AC-20, AC-21
func TestATokenMintedHereIsAnOrdinaryToken(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "ordinary@example.test")
	account := h.accountID(t, cookie)

	raw, _ := h.mintedToken(t, cookie, "codex")

	// It authenticates the deploy path exactly as one minted from /tokens does.
	who, err := h.srv.auth.Authenticate(t.Context(), raw, "203.0.113.9")
	if err != nil {
		t.Fatalf("a token minted from /connect does not authenticate: %v", err)
	}
	if who.ID != account {
		t.Errorf("the token resolved to %s, want %s", who.ID, account)
	}

	// It is on the token list, and /tokens itself gained no blocks of its own.
	list := h.get(t, "/tokens", cookie).Body.String()
	if !strings.Contains(list, "Codex "+h.clock.T.UTC().Format("2006-01-02")) {
		t.Error("a token minted from /connect is not on the token list")
	}
	if strings.Contains(list, "claude mcp add") || strings.Contains(list, "mcp_servers") {
		t.Error("/tokens grew client configuration blocks")
	}

	tokens, err := h.srv.svc.ListTokens(t.Context(), account)
	if err != nil {
		t.Fatalf("listing tokens: %v", err)
	}
	rec := h.postRaw(t, "/tokens/"+tokens[0].ID+"/revoke",
		url.Values{csrfField: {h.csrfFor(t, cookie)}}, nil, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("revoking it from /tokens: got %d, want 303", rec.Code)
	}
	if _, err := h.srv.auth.Authenticate(t.Context(), raw, "203.0.113.9"); err == nil {
		t.Error("the token still authenticates after being revoked on /tokens")
	}
}

// TestARefusedMintRerendersWithNoToken is AC-22. A refusal is the page again
// carrying the refusal's own sentence, never a token and never an error page.
// covers: AC-22
func TestARefusedMintRerendersWithNoToken(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "refused@example.test")
	account := identity.Account{
		ID: h.accountID(t, cookie), Email: "refused@example.test", Verified: true,
	}

	// Every dated name the page would try is already live, which is the one
	// refusal this path can really reach.
	base := "Claude Code " + h.clock.T.UTC().Format("2006-01-02")
	for n := 1; n <= nameOrdinalLimit; n++ {
		name := base
		if n > 1 {
			name = base + " (" + strconv.Itoa(n) + ")"
		}
		if _, err := h.srv.svc.MintToken(t.Context(), account, name, 0); err != nil {
			t.Fatalf("seeding %q: %v", name, err)
		}
	}

	rec := h.postRaw(t, "/connect", url.Values{
		csrfField: {h.csrfFor(t, cookie)}, "client": {"claude-code"},
	}, nil, cookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a refused mint: got %d, want 409: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "you already have a live token by that name") {
		t.Error("the refusal's own sentence is not on the page")
	}
	if got := strings.Count(body, tokenPlaceholder); got != 4 {
		t.Errorf("a refused mint left %d placeholders, want 4", got)
	}
}
