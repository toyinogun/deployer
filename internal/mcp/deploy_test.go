package mcp

import (
	"context"
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
}

func (s *stubApps) ByName(_ context.Context, _, name string) (App, error) {
	if app, ok := s.existing[name]; ok {
		return app, nil
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

// stubDeployments answers with one fixed outcome, so a test can say how the
// deploy ended without running a loop.
type stubDeployments struct {
	created  int
	outcome  Outcome
	release  Release
	lastApp  string
	lastFile string
}

func (s *stubDeployments) Create(_ context.Context, appID, _, uploadID string) (string, error) {
	s.created++
	s.lastApp, s.lastFile = appID, uploadID
	return "dep_1", nil
}

func (s *stubDeployments) Outcome(context.Context, string) (Outcome, error) { return s.outcome, nil }
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
			PublicURL:    "https://deployer.example.org",
			AppDomain:    "deploy.example.org",
			DeployBudget: time.Second,
			PollInterval: time.Millisecond,
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
	deployments := &stubDeployments{
		outcome: Outcome{State: domain.StateHealthy},
		release: Release{Number: 2, Digest: "sha256:abc"},
	}
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
	if out.ReleaseNumber != 2 || out.ImageDigest != "sha256:abc" {
		t.Errorf("release = %d/%s, want 2/sha256:abc", out.ReleaseNumber, out.ImageDigest)
	}
	if out.DeploymentID != "dep_1" || out.Slug != "hello-a1b2c3" || out.Name != "hello" {
		t.Errorf("output = %+v", out)
	}
}

func TestAFirstDeployCreatesTheApp(t *testing.T) {
	account := auth.Account{ID: "acc_1"}
	apps := &stubApps{}
	deployments := &stubDeployments{outcome: Outcome{State: domain.StateHealthy}}
	s, _ := server(apps, deployments, liveUpload(account.ID))

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

// A failed deployment reports its stored code and its one line, and nothing the
// platform saw underneath (AC-16).
func TestAFailedDeployReportsTheCodeAndNothingElse(t *testing.T) {
	account := auth.Account{ID: "acc_1"}
	deployments := &stubDeployments{
		outcome: Outcome{State: domain.StateFailed, Reason: domain.ReasonImageRunsAsRoot},
	}
	s, _ := server(&stubApps{}, deployments, liveUpload(account.ID))

	_, _, err := s.deploy(t.Context(), account, deployInput{Name: "hello", UploadID: "upl_1"})
	if err == nil {
		t.Fatal("want a failure")
	}
	want := string(domain.ReasonImageRunsAsRoot) + ": " + domain.ReasonImageRunsAsRoot.Message()
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
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
	} {
		if !strings.Contains(description, want) {
			t.Errorf("the description does not mention %q", want)
		}
	}
}
