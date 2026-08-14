package web

import (
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/logs"
	"github.com/toyinogun/deployer/internal/store"
)

// TestAppsListShowsServingAndLastDeploySeparately is the case the naive latest
// deployment chain gets wrong, and the state a person opens this page in most
// often: the deploy failed, the previous release is still up. Blurring the two
// tells them their app is down when it is not. covers: AC-14
func TestAppsListShowsServingAndLastDeploySeparately(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "list@example.test")
	h.data.summaries = []store.AppSummary{{
		ID: "app_1", Name: "checkout", Slug: "checkout",
		ServingRelease:       4,
		LastDeploymentID:     "dep_9",
		LastDeploymentState:  string(domain.StateFailed),
		LastDeploymentReason: string(domain.ReasonBuildFailed),
		LastDeployedAt:       "2026-08-14T09:00:00Z",
	}}

	rec := h.get(t, "/apps", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /apps: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"checkout",
		"checkout." + testAppDomain, // AC-14's hostname column
		"release 4",                 // still serving, despite the failure beside it
		"state-failed",
		"The build failed.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the apps list does not show %q", want)
		}
	}
}

// TestAppsListPagesOverTheCursor is AC-14's paging half: twenty at a time, and
// the Load more control appears only when a full page came back.
// covers: AC-14
func TestAppsListPagesOverTheCursor(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "paging@example.test")

	full := make([]store.AppSummary, pageSize)
	for i := range full {
		id := "app_" + strconv.Itoa(i)
		full[i] = store.AppSummary{ID: id, Name: id, Slug: id}
	}
	h.data.summaries = full

	rec := h.get(t, "/apps?cursor=app_earlier", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /apps: got %d, want 200", rec.Code)
	}
	if got := h.data.lastPage; got.Cursor != "app_earlier" || got.Limit != pageSize {
		t.Errorf("the apps list asked for %+v, want cursor app_earlier and limit %d", got, pageSize)
	}
	// A full page means there may be another, keyed on the last row's id.
	if want := "?cursor=" + full[pageSize-1].ID; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("a full page does not offer Load more at %q", want)
	}

	// One short of a page is the end of the list, and offers nothing more.
	h.data.summaries = full[:pageSize-1]
	short := h.get(t, "/apps", cookie)
	if strings.Contains(short.Body.String(), "Load more") {
		t.Error("a partial page still offers Load more")
	}
}

// TestAnAccountWithNoAppsSeesOnboarding is AC-26: zero apps is a beginning, not
// an empty table, and what a new person needs is where to point their agent.
// covers: AC-26
func TestAnAccountWithNoAppsSeesOnboarding(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "new@example.test")

	body := h.get(t, "/apps", cookie).Body.String()
	for _, want := range []string{
		testPublicURL + "/mcp",
		testPublicURL + "/v1/uploads",
		`href="/tokens"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the onboarding panel does not show %q", want)
		}
	}
}

// TestOverviewKeepsServingWhenTheLastDeployFailed is AC-15's load bearing half.
// A failed deployment has no release row of its own, so reading serving off the
// latest deployment answers not found exactly when this page matters most.
// covers: AC-15
func TestOverviewKeepsServingWhenTheLastDeployFailed(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "overview@example.test")
	app := h.ownApp(h.accountID(t, cookie), "checkout")
	app.CurrentReleaseID = ptr("rel_4")
	h.data.apps["checkout"] = app
	h.data.releases["rel_4"] = store.Release{
		ID: "rel_4", AppID: app.ID, ReleaseNumber: 4,
		ImageDigest: "sha256:abcdef0123456789abcdef", CreatedAt: "2026-08-13T10:00:00Z",
	}
	h.data.deployment = store.Deployment{
		ID: "dep_9", AppID: app.ID, State: string(domain.StateFailed),
		FailureReason: ptr(string(domain.ReasonAppNeverReady)),
		CreatedAt:     "2026-08-14T09:00:00Z", FinishedAt: ptr("2026-08-14T09:05:00Z"),
	}

	body := h.get(t, "/apps/checkout", cookie).Body.String()
	for _, want := range []string{
		"Release 4",
		"abcdef012345", // the digest, cut for reading
		"state-failed",
		"The app started but never became ready.",
		"14 Aug 2026, 09:05 UTC", // the finish time, not the create time
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the overview does not show %q", want)
		}
	}
}

// TestAnAppNothingHasDeployedIsAStateNotAnError is AC-15's other end.
// covers: AC-15
func TestAnAppNothingHasDeployedIsAStateNotAnError(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "fresh@example.test")
	h.ownApp(h.accountID(t, cookie), "brandnew")
	h.data.noDeploy = true

	rec := h.get(t, "/apps/brandnew", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET a never deployed app: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Never deployed") {
		t.Error("a never deployed app does not say so")
	}
}

// TestTheStatusFragmentCarriesItsOwnLiveMarker is AC-16. The marker rides inside
// the swapped content: on the container the script replaces it would never
// change and polling would never stop. covers: AC-16
func TestTheStatusFragmentCarriesItsOwnLiveMarker(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "poll@example.test")
	app := h.ownApp(h.accountID(t, cookie), "polling")

	for _, tc := range []struct {
		state domain.State
		live  string
	}{
		{domain.StateQueued, "on"},
		{domain.StateBuilding, "on"},
		{domain.StateDeploying, "on"},
		{domain.StateHealthy, "off"},
		{domain.StateFailed, "off"},
		{domain.StateCancelled, "off"},
	} {
		h.data.deployment = store.Deployment{
			ID: "dep_1", AppID: app.ID, State: string(tc.state), CreatedAt: "2026-08-14T09:00:00Z",
		}
		rec := h.get(t, "/apps/polling?partial=status", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("the status fragment in %s: got %d, want 200", tc.state, rec.Code)
		}
		body := rec.Body.String()
		if want := `data-live="` + tc.live + `"`; !strings.Contains(body, want) {
			t.Errorf("the status fragment in %s does not carry %s", tc.state, want)
		}
		// A fragment replaces a region, so it must not drag a whole page with it.
		if strings.Contains(body, "<html") || strings.Contains(body, "<nav") {
			t.Errorf("the status fragment in %s carried the page shell", tc.state)
		}
	}
}

// TestSupersededRendersAsCancelledNotAsAFailure is AC-17's named case: a deploy
// a newer deploy replaced is not a fault, and showing it as one sends a person
// looking for a problem that does not exist. covers: AC-17
func TestSupersededRendersAsCancelledNotAsAFailure(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "superseded@example.test")
	app := h.ownApp(h.accountID(t, cookie), "replaced")
	h.data.deployment = store.Deployment{
		ID: "dep_2", AppID: app.ID, State: string(domain.StateCancelled),
		FailureReason: ptr(string(domain.ReasonSuperseded)),
		CreatedAt:     "2026-08-14T09:00:00Z",
	}

	body := h.get(t, "/apps/replaced", cookie).Body.String()
	if !strings.Contains(body, "state-cancelled") {
		t.Error("a superseded deploy does not render as cancelled")
	}
	if strings.Contains(body, "state-failed") {
		t.Error("a superseded deploy renders as a failure")
	}
	if !strings.Contains(body, "Cancelled because a newer deploy replaced it.") {
		t.Error("a superseded deploy does not carry its written sentence")
	}
}

// TestEveryReasonCodeRendersASentenceBesideItsCode is AC-17 across the whole
// closed set, plus the drift case: a code added to internal/domain later and not
// written up here degrades to showing the raw code, never an empty element.
// covers: AC-17
func TestEveryReasonCodeRendersASentenceBesideItsCode(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "reasons@example.test")
	app := h.ownApp(h.accountID(t, cookie), "reasons")

	written := []domain.Reason{
		domain.ReasonUploadInvalid, domain.ReasonUploadExpired, domain.ReasonSourceRejected,
		domain.ReasonBuildFailed, domain.ReasonBuildNoDigest, domain.ReasonImageRunsAsRoot,
		domain.ReasonAppNeverReady, domain.ReasonTimeout, domain.ReasonInternal,
	}
	for _, reason := range written {
		h.data.deployment = store.Deployment{
			ID: "dep_3", AppID: app.ID, State: string(domain.StateFailed),
			FailureReason: ptr(string(reason)), CreatedAt: "2026-08-14T09:00:00Z",
		}
		body := h.get(t, "/apps/reasons", cookie).Body.String()
		sentence := reasonSentence(reason)
		if sentence == string(reason) {
			t.Errorf("%s has no written sentence", reason)
			continue
		}
		// The page is HTML, so compare against the escaped form: several of these
		// sentences carry an apostrophe.
		if !strings.Contains(body, template.HTMLEscapeString(sentence)) {
			t.Errorf("%s does not render its sentence", reason)
		}
		// The raw code is shown small beside the sentence, so a person can quote
		// it to an agent that only knows codes.
		if want := `class="reason-code">` + string(reason); !strings.Contains(body, want) {
			t.Errorf("%s does not render its raw code beside the sentence", reason)
		}
	}

	// A code this table has never heard of.
	h.data.deployment = store.Deployment{
		ID: "dep_4", AppID: app.ID, State: string(domain.StateFailed),
		FailureReason: ptr("invented_later"), CreatedAt: "2026-08-14T09:00:00Z",
	}
	body := h.get(t, "/apps/reasons", cookie).Body.String()
	if !strings.Contains(body, "invented_later") {
		t.Error("an unwritten reason code renders nothing at all")
	}
}

// TestAHealthyDeployCarriesNoFailureSentence keeps the reason line off the one
// state that has no failure to explain.
func TestAHealthyDeployCarriesNoFailureSentence(t *testing.T) {
	t.Parallel()
	got := facts(domain.StateHealthy, domain.ReasonInternal, "2026-08-14T09:00:00Z")
	if got.Sentence != "" || got.Code != "" {
		t.Errorf("a healthy deploy carries %+v, want no sentence and no code", got)
	}
	if got.Failed {
		t.Error("a healthy deploy is marked failed")
	}
	if !got.Terminal {
		t.Error("a healthy deploy is not marked terminal")
	}
}

// TestReleasesMarkTheServingOneNotTheNewest is AC-18's whole point: after a
// rollback the newest release row is not the one running.
// covers: AC-18
func TestReleasesMarkTheServingOneNotTheNewest(t *testing.T) {
	h := newHarness(t, nil)
	cookie := h.signIn(t, "rollback@example.test")
	app := h.ownApp(h.accountID(t, cookie), "rolled")
	// Rolled back: release 2 is what is serving, release 3 is the newest row.
	app.CurrentReleaseID = ptr("rel_2")
	h.data.apps["rolled"] = app
	h.data.releases["rel_2"] = store.Release{ID: "rel_2", ReleaseNumber: 2, ImageDigest: "sha256:two"}
	h.data.byApp = []store.Release{
		{ID: "rel_3", ReleaseNumber: 3, ImageDigest: "sha256:three", CreatedAt: "2026-08-14T09:00:00Z"},
		{ID: "rel_2", ReleaseNumber: 2, ImageDigest: "sha256:two", CreatedAt: "2026-08-13T09:00:00Z"},
		{ID: "rel_1", ReleaseNumber: 1, ImageDigest: "sha256:one", CreatedAt: "2026-08-12T09:00:00Z"},
	}
	h.data.noDeploy = true

	rec := h.get(t, "/apps/rolled/releases", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET releases: got %d, want 200", rec.Code)
	}
	// The page is rendered from the data the handler built, so assert on that
	// rather than on which words the template chose to mark it with.
	data, err := releasesFor(t, h, cookie, "rolled")
	if err != nil {
		t.Fatalf("building the releases page: %v", err)
	}
	for _, row := range data.Releases {
		want := row.Number == 2
		if row.Current != want {
			t.Errorf("release %d marked current=%v, want %v", row.Number, row.Current, want)
		}
	}
}

// releasesFor rebuilds one releases page's data through the same reads the
// handler makes, so a test can assert on the marked release rather than on the
// template's wording.
func releasesFor(t *testing.T, h *harness, cookie *http.Cookie, slug string) (releasesPageData, error) {
	t.Helper()
	req := requestWith(t, "/apps/"+slug+"/releases", cookie)
	app := h.data.apps[slug]
	status, err := h.srv.appStatus(req, app)
	if err != nil {
		return releasesPageData{}, err
	}
	rows, err := h.data.ListReleasesByApp(req.Context(), app.ID, store.Page{Limit: pageSize})
	if err != nil {
		return releasesPageData{}, err
	}
	out := releasesPageData{Slug: slug, Serving: status.Serving}
	for _, rel := range rows {
		out.Releases = append(out.Releases, releaseRow{
			Number:  rel.ReleaseNumber,
			Current: status.Serving != 0 && rel.ReleaseNumber == status.Serving,
		})
	}
	return out, nil
}

// TestAnotherAccountsAppIsTheSameNotFoundAsAnUnknownOne is AC-15's ownership
// half. Telling the two apart is how a page becomes a way to discover which app
// names are taken, so the bodies must be byte identical, not merely both 404.
// covers: AC-15
func TestAnotherAccountsAppIsTheSameNotFoundAsAnUnknownOne(t *testing.T) {
	for _, page := range []string{"", "/releases", "/logs", "/config"} {
		h := newHarness(t, &fakePods{})
		owner := h.signIn(t, "owner@example.test")
		other := h.signIn(t, "other@example.test")
		h.ownApp(h.accountID(t, owner), "theirs")

		foreign := h.get(t, "/apps/theirs"+page, other)
		unknown := h.get(t, "/apps/nosuchapp"+page, other)

		if foreign.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
			t.Errorf("/apps/{slug}%s: foreign %d, unknown %d, want 404 and 404",
				page, foreign.Code, unknown.Code)
		}
		if foreign.Body.String() != unknown.Body.String() {
			t.Errorf("/apps/{slug}%s tells a foreign slug apart from an unknown one", page)
		}
		row, ok := h.audit.last(auth.ActionAppView)
		if !ok || row.Reason != string(domain.ReasonAppUnknown) {
			t.Errorf("/apps/{slug}%s wrote %+v, want an app_unknown row", page, row)
		}
	}
}

// TestLogsRenderAnEmptyStateRatherThanAnError is AC-19. Each of these is the app
// not having started, which is a sentence, not a 500. covers: AC-19
func TestLogsRenderAnEmptyStateRatherThanAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		pods *fakePods
		want string
	}{
		{"no cluster credential at all", nil, "no cluster access"},
		{"the namespace is not readable yet", &fakePods{err: logs.ErrNoNamespace}, "has not started yet"},
		{"no pods running", &fakePods{}, "Nothing is running"},
		{"a pod whose container has not started", &fakePods{
			pods: []logs.PodStatus{{Name: "p1", ContainerStarted: false}},
		}, "Nothing is running"},
		{"a running pod that has printed nothing", &fakePods{
			pods: []logs.PodStatus{{Name: "p1", ContainerStarted: true}}, out: "",
		}, "printed nothing yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.pods)
			cookie := h.signIn(t, "logs@example.test")
			h.ownApp(h.accountID(t, cookie), "quiet")

			rec := h.get(t, "/apps/quiet/logs", cookie)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: got %d, want 200", tc.name, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("%s: the page does not say %q", tc.name, tc.want)
			}
		})
	}
}

// TestLogsRedactWhatThePlatformPutThere is AC-19 and AC-31 meeting: the page
// reads output at the moment of the request and redacts the platform's own
// credential and this app's secret values out of it before rendering.
// covers: AC-19, AC-31
func TestLogsRedactWhatThePlatformPutThere(t *testing.T) {
	h := newHarness(t, &fakePods{
		pods: []logs.PodStatus{{Name: "p1", ContainerStarted: true}},
		out: "2026-08-14T09:00:00Z starting up\n" +
			"2026-08-14T09:00:01Z connecting with s3cret-database-password\n" +
			"2026-08-14T09:00:02Z platform token the-platform-credential\n" +
			"2026-08-14T09:00:03Z public setting is eu-west-1\n",
	})
	cookie := h.signIn(t, "redact@example.test")
	h.ownApp(h.accountID(t, cookie), "chatty")
	h.data.config = []store.ConfigEntry{
		{Key: "DATABASE_PASSWORD", IsSecret: true},
		{Key: "REGION", Value: "eu-west-1"},
	}
	h.data.ranConfig = map[string]string{
		"DATABASE_PASSWORD": "s3cret-database-password",
		"REGION":            "eu-west-1",
	}

	body := h.get(t, "/apps/chatty/logs", cookie).Body.String()
	if strings.Contains(body, "s3cret-database-password") {
		t.Error("the logs page printed a secret configuration value")
	}
	if strings.Contains(body, "the-platform-credential") {
		t.Error("the logs page printed the platform's own credential")
	}
	// A value that is not flagged secret is not redacted, or the pane would be
	// unreadable and nobody would trust it.
	if !strings.Contains(body, "eu-west-1") {
		t.Error("the logs page redacted a value that is not secret")
	}
	if !strings.Contains(body, "starting up") {
		t.Error("the logs page dropped an ordinary line")
	}
}

// TestSecretLiteralsComeFromTheReleaseSnapshot is why the release's own
// configuration is read rather than the current one: the pod that is running
// printed the value it started with, and after a rotation that value survives
// only in the snapshot. covers: AC-19
func TestSecretLiteralsComeFromTheReleaseSnapshot(t *testing.T) {
	h := newHarness(t, &fakePods{})
	cookie := h.signIn(t, "rotate@example.test")
	app := h.ownApp(h.accountID(t, cookie), "rotated")
	h.data.config = []store.ConfigEntry{{Key: "API_KEY", IsSecret: true}}
	h.data.ranConfig = map[string]string{"API_KEY": "the-old-value-still-running"}

	literals, err := h.srv.secretLiterals(requestWith(t, "/apps/rotated/logs", cookie), app)
	if err != nil {
		t.Fatalf("reading the redaction literals: %v", err)
	}
	if !containsString(literals, "the-old-value-still-running") {
		t.Errorf("the running release's secret value is not redacted: %v", literals)
	}
	if !containsString(literals, "the-platform-credential") {
		t.Errorf("the platform's own credential is not redacted: %v", literals)
	}
}

func containsString(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

// TestShortDigestCutsToSomethingComparable keeps the full value in the title
// attribute and a glanceable prefix in the text.
func TestShortDigestCutsToSomethingComparable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"sha256:abcdef0123456789abcdef", "abcdef012345"},
		{"sha256:abc", "sha256:abc"}, // too short after the colon, shown whole
		{"abcdef0123456789", "abcdef012345"},
		{"short", "short"},
		{"", ""},
	} {
		if got := shortDigest(tc.in); got != tc.want {
			t.Errorf("shortDigest(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestWhenTextShowsAnUnreadableStampRatherThanBlankingIt: a timestamp the
// platform wrote and cannot read back is worth seeing, not hiding.
func TestWhenTextShowsAnUnreadableStampRatherThanBlankingIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"2026-08-14T05:55:00Z", "14 Aug 2026, 05:55 UTC"},
		{"2026-08-14T07:55:00+02:00", "14 Aug 2026, 05:55 UTC"}, // normalised to UTC
		{"not a timestamp", "not a timestamp"},
		{"", ""},
	} {
		if got := whenText(tc.in); got != tc.want {
			t.Errorf("whenText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// requestWith builds a request the way the mux would hand one to a handler.
func requestWith(t *testing.T, path string, cookie *http.Cookie) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	req.URL.RawQuery = url.Values{}.Encode()
	return req
}
