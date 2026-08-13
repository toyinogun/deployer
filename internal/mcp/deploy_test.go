package mcp

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
)

// stubApps records whether a call created an app, which is what says a refused
// deploy touched nothing.
type stubApps struct {
	existing map[string]App
	created  []string
	// readErr stands in for a store fault rather than a missing row, which is a
	// different answer for a caller.
	readErr error
	// config is the app's stored configuration, keyed by app id then by key. It
	// is a map rather than a real store because what these tests exercise is the
	// tool surface; the transactional half is proven against real SQLite in
	// internal/store.
	config map[string]map[string]ConfigEntry
	// released is what the app's current release ran with, which is the second
	// half of what get_logs redacts against.
	released map[string]string
}

func (s *stubApps) ReleaseConfig(_ context.Context, _ string) (map[string]string, error) {
	return s.released, nil
}

func (s *stubApps) Config(_ context.Context, appID string) ([]ConfigEntry, error) {
	entries := s.configEntries(appID)
	// The response path never carries a secret value, which is the store's job
	// in production and has to be the stub's here too (spec 0010, AC-2).
	for i, e := range entries {
		if e.Secret {
			entries[i].Value = ""
		}
	}
	return entries, nil
}

func (s *stubApps) ConfigValues(_ context.Context, appID string) ([]ConfigEntry, error) {
	return s.configEntries(appID), nil
}

func (s *stubApps) configEntries(appID string) []ConfigEntry {
	entries := make([]ConfigEntry, 0, len(s.config[appID]))
	for _, e := range s.config[appID] {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries
}

func (s *stubApps) SetConfig(_ context.Context, appID string, entries []ConfigEntry) error {
	if s.config == nil {
		s.config = map[string]map[string]ConfigEntry{}
	}
	if s.config[appID] == nil {
		s.config[appID] = map[string]ConfigEntry{}
	}
	for _, e := range entries {
		s.config[appID][e.Key] = e
	}
	return nil
}

func (s *stubApps) UnsetConfig(_ context.Context, appID string, keys []string) error {
	// All or nothing, exactly as the transaction behind the real one is.
	for _, k := range keys {
		if _, ok := s.config[appID][k]; !ok {
			return ErrNoConfigKey
		}
	}
	for _, k := range keys {
		delete(s.config[appID], k)
	}
	return nil
}

func (s *stubApps) ByName(_ context.Context, _, name string) (App, error) {
	if s.readErr != nil {
		return App{}, s.readErr
	}
	if app, ok := s.existing[name]; ok {
		return app, nil
	}
	return App{}, ErrNoApp
}

func (s *stubApps) Get(_ context.Context, appID string) (App, error) {
	for _, app := range s.existing {
		if app.ID == appID {
			return app, nil
		}
	}
	return App{}, ErrNoApp
}

func (s *stubApps) Create(_ context.Context, _, name string) (App, error) {
	s.created = append(s.created, name)
	app := App{ID: "app_2", Slug: "new-b2c3d4", Name: name}
	if s.existing == nil {
		s.existing = map[string]App{}
	}
	s.existing[name] = app
	return app, nil
}

// stubDeployments answers with fixed rows, so a test can say where a deployment
// got to without running a loop.
type stubDeployments struct {
	created  int
	rows     map[string]Deployment
	latest   Deployment
	next     string
	events   []Event
	release  Release
	lastApp  string
	lastFile string
	// readErr stands in for a store fault rather than a missing row.
	readErr error
}

func (s *stubDeployments) Create(_ context.Context, appID, _, uploadID string) (string, error) {
	s.created++
	s.lastApp, s.lastFile = appID, uploadID
	return "dep_1", nil
}

func (s *stubDeployments) Get(_ context.Context, id string) (Deployment, error) {
	if s.readErr != nil {
		return Deployment{}, s.readErr
	}
	dep, ok := s.rows[id]
	if !ok {
		return Deployment{}, ErrNoDeployment
	}
	return dep, nil
}

func (s *stubDeployments) LatestForApp(context.Context, string) (Deployment, error) {
	if s.readErr != nil {
		return Deployment{}, s.readErr
	}
	if s.latest.ID == "" {
		return Deployment{}, ErrNoDeployment
	}
	return s.latest, nil
}

func (s *stubDeployments) NextForApp(context.Context, string, string) (string, error) {
	return s.next, nil
}

func (s *stubDeployments) Events(context.Context, string) ([]Event, error)  { return s.events, nil }
func (s *stubDeployments) Release(context.Context, string) (Release, error) { return s.release, nil }

// stubUploads holds the one upload a test offers.
type stubUploads struct{ up Upload }

func (s stubUploads) Get(_ context.Context, id string) (Upload, error) {
	if s.up.ID != id {
		return Upload{}, ErrNoUpload
	}
	return s.up, nil
}

// silentAuditor accepts audit rows without keeping them.
type silentAuditor struct{ rows []auth.Audit }

func (a *silentAuditor) RecordAudit(_ context.Context, e auth.Audit) error {
	a.rows = append(a.rows, e)
	return nil
}

// server builds a tool surface over the stubs.
func server(apps Apps, deployments Deployments, up Upload) (*Server, *silentAuditor) {
	auditor := &silentAuditor{}
	return &Server{
		auditor:     auditor,
		apps:        apps,
		deployments: deployments,
		uploads:     stubUploads{up: up},
		opts: Options{
			PublicURL: "https://deployer.example.org",
			AppDomain: "deploy.example.org",
		},
	}, auditor
}

// liveUpload is an upload that is this account's, unspent, and in date.
func liveUpload(accountID string) Upload {
	return Upload{
		ID:        "upl_1",
		AccountID: accountID,
		ExpiresAt: ids.Stamp(time.Now().Add(time.Hour)),
	}
}

func TestASuccessfulDeployReportsWhatWasWritten(t *testing.T) {
	account := auth.Account{ID: "acc_1", Name: "bootstrap"}
	apps := &stubApps{existing: map[string]App{"hello": {ID: "app_1", Slug: "hello-a1b2c3", Name: "hello"}}}
	deployments := &stubDeployments{}
	s, _ := server(apps, deployments, liveUpload(account.ID))

	_, out, err := s.deploy(t.Context(), account, deployInput{Name: "hello", UploadID: "upl_1"})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// An app that already exists is reused, which is what keeps the hostname an
	// agent already handed someone working (AC-4).
	if len(apps.created) != 0 {
		t.Errorf("created %v, want the existing app reused", apps.created)
	}
	if out.URL != "https://hello-a1b2c3.deploy.example.org" {
		t.Errorf("url = %q", out.URL)
	}
	// The row was just written queued, and nothing is read back (AC-2).
	if out.State != string(domain.StateQueued) {
		t.Errorf("state = %q, want queued", out.State)
	}
	if out.DeploymentID != "dep_1" || out.Slug != "hello-a1b2c3" || out.Name != "hello" {
		t.Errorf("output = %+v", out)
	}
}

func TestAFirstDeployCreatesTheApp(t *testing.T) {
	account := auth.Account{ID: "acc_1"}
	apps := &stubApps{}
	s, _ := server(apps, &stubDeployments{}, liveUpload(account.ID))

	if _, _, err := s.deploy(t.Context(), account, deployInput{Name: "new", UploadID: "upl_1"}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(apps.created) != 1 || apps.created[0] != "new" {
		t.Errorf("created = %v, want [new]", apps.created)
	}
}

// A bad upload must fail before the app is touched, so the audit row has a null
// target and no app row is created (AC-19).
func TestABadUploadTouchesNothing(t *testing.T) {
	account := auth.Account{ID: "acc_1"}
	cases := map[string]Upload{
		"unknown":         {ID: "other", AccountID: account.ID, ExpiresAt: ids.Stamp(time.Now().Add(time.Hour))},
		"another account": {ID: "upl_1", AccountID: "acc_2", ExpiresAt: ids.Stamp(time.Now().Add(time.Hour))},
		"already spent":   {ID: "upl_1", AccountID: account.ID, ExpiresAt: ids.Stamp(time.Now().Add(time.Hour)), Redeemed: true},
	}
	for name, up := range cases {
		t.Run(name, func(t *testing.T) {
			apps := &stubApps{}
			deployments := &stubDeployments{}
			s, auditor := server(apps, deployments, up)

			_, _, err := s.deploy(t.Context(), account, deployInput{Name: "hello", UploadID: "upl_1"})
			if err == nil || !strings.HasPrefix(err.Error(), string(domain.ReasonUploadInvalid)) {
				t.Fatalf("error = %v, want %s", err, domain.ReasonUploadInvalid)
			}
			if len(apps.created) != 0 || deployments.created != 0 {
				t.Errorf("created %v apps and %d deployments, want none", apps.created, deployments.created)
			}
			if len(auditor.rows) != 1 || auditor.rows[0].TargetID != "" || auditor.rows[0].Allowed {
				t.Errorf("audit = %+v, want one denial with no target", auditor.rows)
			}
		})
	}
}

func TestAnExpiredUploadSaysSo(t *testing.T) {
	account := auth.Account{ID: "acc_1"}
	up := liveUpload(account.ID)
	up.ExpiresAt = ids.Stamp(time.Now().Add(-time.Minute))
	s, _ := server(&stubApps{}, &stubDeployments{}, up)

	_, _, err := s.deploy(t.Context(), account, deployInput{Name: "hello", UploadID: "upl_1"})
	if err == nil || !strings.HasPrefix(err.Error(), string(domain.ReasonUploadExpired)) {
		t.Fatalf("error = %v, want %s", err, domain.ReasonUploadExpired)
	}
}

// The tool description is part of the contract: an agent that cannot read the
// upload step out of it cannot deploy at all.
func TestTheToolDescriptionCarriesTheUploadContract(t *testing.T) {
	s, _ := server(&stubApps{}, &stubDeployments{}, Upload{})
	description := s.toolDescription()

	for _, want := range []string{
		"https://deployer.example.org/v1/uploads",
		"tar czf",
		"Authorization: Bearer",
		"PORT",
		"non root",
		"minutes",
		"deployment_status",
		"queued",
		// The async contract itself, not only the tool that reads it back
		// (spec 0005, AC-4).
		"straight away",
		"does not wait",
		// The detection rule, which is the only place an agent learns that a
		// root Dockerfile changes how its app is built (spec 0009, AC-16).
		"Dockerfile at the root",
		"Buildpacks",
		"build_path",
	} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not mention %q", want)
		}
	}
}
