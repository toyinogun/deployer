package web

import (
	"errors"
	"net/http"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/deploy"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/logs"
	"github.com/toyinogun/deployer/internal/store"
)

// pageSize is how many rows one list page carries. Twenty is what fits a screen
// without scrolling past the point a person stops reading, and the Load more
// control makes the rest reachable (AC-14, AC-18).
const pageSize = 20

// appRow is one line of the app list.
type appRow struct {
	Name     string
	Slug     string
	Hostname string
	// Serving and LastDeploy are kept apart on purpose. An app whose most recent
	// deploy failed is usually still serving its previous release, and blurring
	// the two into one state is exactly how a page tells that person their app
	// is down when it is not (spec 0012, AC-5).
	Serving    int64
	LastDeploy deployFacts
}

// deployFacts is how a deployment ended, as a page shows it.
type deployFacts struct {
	Present bool
	State   string
	// Terminal decides whether the page polls at all. Read from the same state
	// machine the reconcile loop drives, never from a list of state names a page
	// keeps for itself.
	Terminal bool
	Failed   bool
	// Sentence is the plain line written for the failure's reason code, and Code
	// is that code shown beside it.
	Sentence string
	Code     string
	When     string
}

// appsPage is the list. Every row belongs to the signed in account, because the
// query is account scoped: there is no filter here to forget (AC-14).
type appsPageData struct {
	Apps       []appRow
	NextCursor string
	// MCPEndpoint and UploadEndpoint are shown to an account with no apps yet,
	// so the first thing a new person sees is how to point an agent at the
	// platform rather than an empty table (AC-26).
	MCPEndpoint    string
	UploadEndpoint string
	// AppsUsed and AppLimit are the account's live app count and its ceiling,
	// read from the same count deploy_app is refused against. AtLimit is derived
	// at render time rather than stored (spec 0016, AC-10, AC-11).
	AppsUsed int
	AppLimit int
	AtLimit  bool
}

func (s *Server) appsPage(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	rows, err := s.data.ListAppSummaryPage(r.Context(), account.ID,
		store.Page{Cursor: r.URL.Query().Get("cursor"), Limit: pageSize})
	if err != nil {
		s.internalError(w, r, err, "listing apps for a page failed")
		return
	}

	// Counted rather than taken from len(rows): the list is one page, and the
	// usage line is about every app the account holds (AC-10).
	used, err := s.data.CountLiveAppsByAccount(r.Context(), account.ID)
	if err != nil {
		s.internalError(w, r, err, "counting an account's apps for a page failed")
		return
	}

	data := appsPageData{
		Apps:           make([]appRow, 0, len(rows)),
		MCPEndpoint:    s.opts.MCPURL + "/mcp",
		UploadEndpoint: s.opts.MCPURL + "/v1/uploads",
		AppsUsed:       used,
		AppLimit:       s.opts.MaxAppsPerAccount,
		AtLimit:        used >= s.opts.MaxAppsPerAccount,
	}
	for _, row := range rows {
		data.Apps = append(data.Apps, appRow{
			Name:       row.Name,
			Slug:       row.Slug,
			Hostname:   s.hostname(row.Slug),
			Serving:    row.ServingRelease,
			LastDeploy: factsFromSummary(row),
		})
	}
	// A full page means there may be another. An empty next cursor is what the
	// template reads to leave the Load more control out.
	if len(rows) == pageSize {
		data.NextCursor = rows[len(rows)-1].ID
	}
	s.render(w, r, account, sess, http.StatusOK, "apps", "apps", data)
}

// appPageData is the overview.
type appPageData struct {
	Name     string
	Slug     string
	Hostname string
	Status   appStatus
}

// appStatus is the region the page polls: everything about the app that a deploy
// in flight can change, and nothing that it cannot.
type appStatus struct {
	Slug string
	// Serving is the release the app is actually serving, read from the app's
	// own current release rather than from the latest deployment. A failed
	// deployment has no release row of its own, so the deployment chain answers
	// not found exactly when this page matters most (AC-15).
	Serving       int64
	ServingDigest string
	ServingSince  string
	// NeverDeployed is an app nothing has ever deployed, which is a state rather
	// than an error.
	NeverDeployed bool
	LastDeploy    deployFacts
}

func (s *Server) appPage(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	app, ok := s.ownedApp(w, r, account, sess)
	if !ok {
		return
	}
	status, err := s.appStatus(r, app)
	if err != nil {
		s.internalError(w, r, err, "reading an app's status failed")
		return
	}
	// The polled fragment is the status region alone, rendered from the same
	// value the whole page renders it from, so a poll and a reload can never
	// disagree about what the app is doing (AC-16).
	if r.URL.Query().Get("partial") == "status" {
		s.writeFragment(w, r, "app", "status", status)
		return
	}
	s.render(w, r, account, sess, http.StatusOK, "app", "apps", appPageData{
		Name:     app.Name,
		Slug:     app.Slug,
		Hostname: s.hostname(app.Slug),
		Status:   status,
	})
}

// appStatus reads the two independent facts: what the app is serving, and how
// its last deployment ended.
func (s *Server) appStatus(r *http.Request, app store.App) (appStatus, error) {
	out := appStatus{Slug: app.Slug}

	if releaseID := app.CurrentReleaseID; releaseID != nil && *releaseID != "" {
		rel, err := s.data.GetRelease(r.Context(), *releaseID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return appStatus{}, err
		}
		if err == nil {
			out.Serving = rel.ReleaseNumber
			out.ServingDigest = rel.ImageDigest
			out.ServingSince = rel.CreatedAt
		}
	}

	dep, err := s.data.GetLatestDeploymentForApp(r.Context(), app.ID)
	if errors.Is(err, store.ErrNotFound) {
		out.NeverDeployed = true
		return out, nil
	}
	if err != nil {
		return appStatus{}, err
	}
	out.LastDeploy = factsFromDeployment(dep)
	return out, nil
}

// releasesPageData is the release history.
type releasesPageData struct {
	Slug       string
	Name       string
	Serving    int64
	Releases   []releaseRow
	NextCursor string
}

// releaseRow is one release, marked current from the same serving fact the
// overview reads, never from being the newest row: the newest release is not
// the one running after a rollback (AC-18).
type releaseRow struct {
	Number  int64
	Digest  string
	Created string
	Current bool
}

func (s *Server) releasesPage(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	app, ok := s.ownedApp(w, r, account, sess)
	if !ok {
		return
	}
	status, err := s.appStatus(r, app)
	if err != nil {
		s.internalError(w, r, err, "reading an app's serving release failed")
		return
	}
	rows, err := s.data.ListReleasesByApp(r.Context(), app.ID,
		store.Page{Cursor: r.URL.Query().Get("cursor"), Limit: pageSize})
	if err != nil {
		s.internalError(w, r, err, "listing releases for a page failed")
		return
	}

	data := releasesPageData{
		Slug: app.Slug, Name: app.Name, Serving: status.Serving,
		Releases: make([]releaseRow, 0, len(rows)),
	}
	for _, rel := range rows {
		data.Releases = append(data.Releases, releaseRow{
			Number:  rel.ReleaseNumber,
			Digest:  rel.ImageDigest,
			Created: rel.CreatedAt,
			Current: status.Serving != 0 && rel.ReleaseNumber == status.Serving,
		})
	}
	if len(rows) == pageSize {
		data.NextCursor = rows[len(rows)-1].ID
	}
	s.render(w, r, account, sess, http.StatusOK, "releases", "apps", data)
}

// logsPageData is the output pane.
type logsPageData struct {
	Slug    string
	Name    string
	Entries []logs.Entry
	// Empty is the sentence shown when there is nothing to show, which is a
	// different thing from something being broken (AC-19, AC-26).
	Empty     string
	Truncated bool
}

// logsPage reads the app's output from Kubernetes at the moment of the request,
// redacts it, bounds it, and renders it. Nothing is written anywhere: no table,
// no file, and no line at info level in the platform's own log (AC-19).
func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	app, ok := s.ownedApp(w, r, account, sess)
	if !ok {
		return
	}
	data := logsPageData{Slug: app.Slug, Name: app.Name}

	if s.pods == nil {
		data.Empty = "The platform has no cluster access right now, so there is no output to read."
		s.render(w, r, account, sess, http.StatusOK, "logs", "apps", data)
		return
	}

	namespace := deploy.NamespaceName(app.Slug)
	pods, err := s.pods.PodsForApp(r.Context(), namespace, app.Slug)
	switch {
	// A namespace the platform cannot read yet is the app not having started,
	// which is an explanatory empty state rather than a failure: the namespace
	// is created at the deploy step, so this is what a queued or building app
	// looks like from here.
	case errors.Is(err, logs.ErrNoNamespace):
		data.Empty = "This app has not started yet, so it has printed nothing."
		s.render(w, r, account, sess, http.StatusOK, "logs", "apps", data)
		return
	case err != nil:
		s.internalError(w, r, err, "listing the pods of an app failed")
		return
	}
	if len(pods) == 0 || !pods[0].ContainerStarted {
		data.Empty = "Nothing is running for this app right now, so there is no output to show."
		s.render(w, r, account, sess, http.StatusOK, "logs", "apps", data)
		return
	}

	literals, err := s.secretLiterals(r, app)
	if err != nil {
		s.internalError(w, r, err, "reading an app's configuration for redaction failed")
		return
	}
	raw, err := s.pods.PodLog(r.Context(), namespace, pods[0].Name, logs.BrowserTail, false)
	if err != nil {
		s.internalError(w, r, err, "reading an app's output failed")
		return
	}
	// Redaction runs on every line before bounding, so what is dropped is
	// dropped from output that is already safe to show.
	entries, dropped := logs.Bound(
		logs.RedactAll(logs.Parse(raw), literals), logs.BrowserTail, logs.BrowserBytes)
	data.Entries, data.Truncated = entries, dropped > 0
	if len(entries) == 0 {
		data.Empty = "This app is running but has printed nothing yet."
	}
	s.render(w, r, account, sess, http.StatusOK, "logs", "apps", data)
}

// secretLiterals is every string this app's output must not carry: the platform's
// own credential, and the values the running release was deployed with for the
// keys that are secret today.
//
// The release's configuration rather than the current one is what covers a
// rotation: the pod that is running printed the old value, and once the key has
// been set again the old value survives only in the release snapshot. It is also
// what keeps this page off ListConfigForDeploy, which keeps exactly two callers
// (spec 0013, Key invariants).
func (s *Server) secretLiterals(r *http.Request, app store.App) ([]string, error) {
	entries, err := s.data.ListConfigForResponse(r.Context(), app.ID)
	if err != nil {
		return nil, err
	}
	secretNow := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsSecret {
			secretNow[e.Key] = struct{}{}
		}
	}
	ran, err := s.data.CurrentReleaseConfig(r.Context(), app.ID)
	if err != nil {
		return nil, err
	}
	literals := make([]string, 0, len(s.opts.SecretLiterals)+len(secretNow))
	literals = append(literals, s.opts.SecretLiterals...)
	for key, value := range ran {
		if _, secret := secretNow[key]; secret {
			literals = append(literals, value)
		}
	}
	return literals, nil
}

// configPageData is the configuration listing.
type configPageData struct {
	Slug string
	Name string
	Keys []configRow
}

// configRow is one key. A secret key carries no value and renders no reveal
// control at all, so there is no route in this package that can return one
// (AC-20).
type configRow struct {
	Key      string
	Set      bool
	Secret   bool
	Revealed string
}

func (s *Server) configPage(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	app, ok := s.ownedApp(w, r, account, sess)
	if !ok {
		return
	}
	data, err := s.configData(r, app)
	if err != nil {
		s.internalError(w, r, err, "reading an app's configuration failed")
		return
	}
	s.render(w, r, account, sess, http.StatusOK, "config", "apps", data)
}

// configReveal shows one value back, for a key that is not flagged secret, and
// writes an audit row naming the account, the app and the key either way.
//
// A secret key is refused here as well as having no control rendered for it: the
// missing button is a display decision, and this is the one that holds.
func (s *Server) configReveal(w http.ResponseWriter, r *http.Request) {
	account, sess, ok := s.session(w, r)
	if !ok {
		return
	}
	app, ok := s.ownedApp(w, r, account, sess)
	if !ok {
		return
	}
	if !s.checkCSRF(w, r, account, sess) {
		return
	}
	key := r.PathValue("key")

	entries, err := s.data.ListConfigForResponse(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err, "reading an app's configuration failed")
		return
	}
	var found *store.ConfigEntry
	for i := range entries {
		if entries[i].Key == key {
			found = &entries[i]
			break
		}
	}
	if found == nil || found.IsSecret {
		reason := string(domain.ReasonConfigKeyUnknown)
		if found != nil {
			reason = "config_key_secret"
		}
		auth.Record(r.Context(), s.auditor, auth.Audit{
			ClientAddress: s.clientAddress(r),
			AccountID:     account.ID, Action: auth.ActionConfigReveal,
			TargetType: auth.TargetAppConfig, TargetID: app.ID + "/" + key, Reason: reason,
		})
		s.renderRefused(w, r, account, sess, http.StatusForbidden, "That value cannot be shown",
			"Secret values are never readable in the browser, the same as through an agent.")
		return
	}
	auth.Record(r.Context(), s.auditor, auth.Audit{
		ClientAddress: s.clientAddress(r),
		AccountID:     account.ID, Action: auth.ActionConfigReveal, Allowed: true,
		TargetType: auth.TargetAppConfig, TargetID: app.ID + "/" + key,
	})

	data, err := s.configData(r, app)
	if err != nil {
		s.internalError(w, r, err, "reading an app's configuration failed")
		return
	}
	for i := range data.Keys {
		if data.Keys[i].Key == key {
			data.Keys[i].Revealed = found.Value
		}
	}
	s.render(w, r, account, sess, http.StatusOK, "config", "apps", data)
}

// configData is the listing both the page and the reveal render from.
func (s *Server) configData(r *http.Request, app store.App) (configPageData, error) {
	entries, err := s.data.ListConfigForResponse(r.Context(), app.ID)
	if err != nil {
		return configPageData{}, err
	}
	data := configPageData{Slug: app.Slug, Name: app.Name, Keys: make([]configRow, 0, len(entries))}
	for _, e := range entries {
		data.Keys = append(data.Keys, configRow{
			Key: e.Key, Secret: e.IsSecret, Set: e.IsSecret || e.Value != "",
		})
	}
	return data, nil
}

// ownedApp resolves the slug in the path to an app the signed in account owns.
//
// A slug belonging to someone else and a slug that does not exist answer the
// same page, and both write an audit row: telling the two apart is how a page
// becomes a way to discover which app names are taken (AC-15).
func (s *Server) ownedApp(w http.ResponseWriter, r *http.Request, account auth.Account, sess auth.Session) (store.App, bool) {
	slug := r.PathValue("slug")
	app, err := s.data.GetAppBySlug(r.Context(), slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err, "reading an app for a page failed")
		return store.App{}, false
	}
	if err != nil || app.AccountID != account.ID {
		auth.Record(r.Context(), s.auditor, auth.Audit{
			ClientAddress: s.clientAddress(r),
			AccountID:     account.ID, Action: auth.ActionAppView,
			TargetType: "app", TargetID: slug, Reason: string(domain.ReasonAppUnknown),
		})
		s.renderRefused(w, r, account, sess, http.StatusNotFound, "No such app",
			"There is no app by that name on this account.")
		return store.App{}, false
	}
	return app, true
}

// factsFromSummary reads how a listed app's last deploy ended.
func factsFromSummary(row store.AppSummary) deployFacts {
	if row.LastDeploymentID == "" {
		return deployFacts{}
	}
	return facts(domain.State(row.LastDeploymentState), domain.Reason(row.LastDeploymentReason), row.LastDeployedAt)
}

// factsFromDeployment reads the same from the deployment row itself.
func factsFromDeployment(dep store.Deployment) deployFacts {
	when := dep.CreatedAt
	if dep.FinishedAt != nil && *dep.FinishedAt != "" {
		when = *dep.FinishedAt
	}
	var reason domain.Reason
	if dep.FailureReason != nil {
		reason = domain.Reason(*dep.FailureReason)
	}
	return facts(domain.State(dep.State), reason, when)
}

// facts turns a state and a reason into what a page shows for them.
//
// A superseded deployment is cancelled, not failed: a deploy that a newer deploy
// replaced is not a fault, and the state machine already says so by ending it in
// cancelled rather than failed (AC-17).
func facts(state domain.State, reason domain.Reason, when string) deployFacts {
	out := deployFacts{
		Present:  true,
		State:    string(state),
		Terminal: state.Terminal(),
		Failed:   state == domain.StateFailed,
		When:     when,
	}
	if reason != "" && state != domain.StateHealthy {
		out.Code = string(reason)
		out.Sentence = reasonSentence(reason)
	}
	return out
}
