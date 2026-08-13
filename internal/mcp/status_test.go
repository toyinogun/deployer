package mcp

import (
	"errors"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
)

// statusServer builds a tool surface over one app and its deployments.
func statusServer(deployments *stubDeployments) (*Server, *silentAuditor) {
	apps := &stubApps{existing: map[string]App{
		"hello": {ID: "app_1", Slug: "hello-a1b2c3", Name: "hello"},
	}}
	return server(apps, deployments, Upload{})
}

// unknown is the one answer every refused status read gets.
var unknown = string(domain.ReasonDeploymentUnknown) + ": " + domain.ReasonDeploymentUnknown.Message()

func TestStatusNeedsExactlyOneOfIdAndName(t *testing.T) {
	// covers: AC-5
	t.Parallel()
	account := auth.Account{ID: "acc_1"}
	for name, in := range map[string]statusInput{
		"neither": {},
		"both":    {DeploymentID: "dep_1", Name: "hello"},
	} {
		t.Run(name, func(t *testing.T) {
			deployments := &stubDeployments{rows: map[string]Deployment{
				"dep_1": {ID: "dep_1", AppID: "app_1", AccountID: "acc_1", State: domain.StateQueued},
			}}
			s, auditor := statusServer(deployments)

			_, _, err := s.status(t.Context(), account, in)
			if err == nil || err.Error() != unknown {
				t.Fatalf("error = %v, want %q", err, unknown)
			}
			// Refused before anything is read, and audited exactly once (AC-10).
			if len(auditor.rows) != 1 || auditor.rows[0].Action != auth.ActionStatus || auditor.rows[0].Allowed {
				t.Errorf("audit = %+v, want one status denial", auditor.rows)
			}
		})
	}
}

func TestStatusAnswersUnknownAndAnotherAccountIdentically(t *testing.T) {
	// covers: AC-9, AC-10
	t.Parallel()
	account := auth.Account{ID: "acc_1"}
	deployments := &stubDeployments{rows: map[string]Deployment{
		"dep_other": {ID: "dep_other", AppID: "app_9", AccountID: "acc_2", State: domain.StateHealthy},
	}}
	for name, in := range map[string]statusInput{
		"unknown id":         {DeploymentID: "dep_nope"},
		"another account id": {DeploymentID: "dep_other"},
		"unknown name":       {Name: "nothing-here"},
		"app never deployed": {Name: "hello"},
	} {
		t.Run(name, func(t *testing.T) {
			s, auditor := statusServer(deployments)
			_, _, err := s.status(t.Context(), account, in)
			if err == nil || err.Error() != unknown {
				t.Fatalf("error = %v, want %q", err, unknown)
			}
			if len(auditor.rows) != 1 {
				t.Errorf("audit = %+v, want exactly one denial", auditor.rows)
			}
		})
	}
}

func TestStatusReportsAStoreFaultAsInternalRatherThanUnknown(t *testing.T) {
	// covers: AC-9, AC-10. A fault is not a refusal: telling a polling agent its
	// id is wrong would stop it polling a deployment that is still running, and
	// the denial would land in audit_log as an access decision that never happened.
	t.Parallel()
	account := auth.Account{ID: "acc_1"}
	fault := errors.New("the database is busy")
	internal := string(domain.ReasonInternal) + ": " + domain.ReasonInternal.Message()

	for name, tc := range map[string]struct {
		in    statusInput
		apps  *stubApps
		reads *stubDeployments
	}{
		"reading the deployment by id": {
			in:    statusInput{DeploymentID: "dep_1"},
			apps:  &stubApps{existing: map[string]App{"hello": {ID: "app_1", Name: "hello"}}},
			reads: &stubDeployments{readErr: fault},
		},
		"reading the app by name": {
			in:    statusInput{Name: "hello"},
			apps:  &stubApps{readErr: fault},
			reads: &stubDeployments{},
		},
		"reading the app's latest deployment": {
			in:    statusInput{Name: "hello"},
			apps:  &stubApps{existing: map[string]App{"hello": {ID: "app_1", Name: "hello"}}},
			reads: &stubDeployments{readErr: fault},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, auditor := server(tc.apps, tc.reads, Upload{})

			_, _, err := s.status(t.Context(), account, tc.in)
			if err == nil || err.Error() != internal {
				t.Fatalf("error = %v, want %q", err, internal)
			}
			if len(auditor.rows) != 0 {
				t.Errorf("audit = %+v, want no row: a fault is not an access decision", auditor.rows)
			}
		})
	}
}

func TestStatusPayloadCarriesWhatTheStateHas(t *testing.T) {
	// covers: AC-6, AC-7, AC-8, AC-10
	t.Parallel()
	account := auth.Account{ID: "acc_1"}
	base := Deployment{ID: "dep_1", AppID: "app_1", AccountID: "acc_1"}

	t.Run("healthy", func(t *testing.T) {
		dep := base
		dep.State = domain.StateHealthy
		deployments := &stubDeployments{
			rows:    map[string]Deployment{"dep_1": dep},
			release: Release{Number: 2, Digest: "sha256:abc"},
			events: []Event{
				{State: domain.StateQueued, At: "2026-08-12T10:00:00Z"},
				{State: domain.StateHealthy, At: "2026-08-12T10:04:00Z"},
			},
		}
		s, auditor := statusServer(deployments)

		_, out, err := s.status(t.Context(), account, statusInput{DeploymentID: "dep_1"})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if out.URL != "https://hello-a1b2c3.deploy.example.org" || out.AppName != "hello" || out.Slug != "hello-a1b2c3" {
			t.Errorf("output = %+v", out)
		}
		if out.ReleaseNumber != 2 || out.ImageDigest != "sha256:abc" {
			t.Errorf("release = %d/%s, want 2/sha256:abc", out.ReleaseNumber, out.ImageDigest)
		}
		if len(out.Timeline) != 2 || out.Timeline[0].State != "queued" || out.Timeline[1].At != "2026-08-12T10:04:00Z" {
			t.Errorf("timeline = %+v", out.Timeline)
		}
		// An allowed read is not audited (AC-10).
		if len(auditor.rows) != 0 {
			t.Errorf("audit = %+v, want none", auditor.rows)
		}
	})

	t.Run("queued carries no release", func(t *testing.T) {
		dep := base
		dep.State = domain.StateQueued
		s, _ := statusServer(&stubDeployments{
			rows:    map[string]Deployment{"dep_1": dep},
			release: Release{Number: 9, Digest: "sha256:never"},
		})
		_, out, err := s.status(t.Context(), account, statusInput{DeploymentID: "dep_1"})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if out.ReleaseNumber != 0 || out.ImageDigest != "" || out.Reason != "" {
			t.Errorf("output = %+v, want state only", out)
		}
	})

	t.Run("failed carries the code and its one line", func(t *testing.T) {
		dep := base
		dep.State, dep.Reason = domain.StateFailed, domain.ReasonImageRunsAsRoot
		s, _ := statusServer(&stubDeployments{rows: map[string]Deployment{"dep_1": dep}})

		_, out, err := s.status(t.Context(), account, statusInput{DeploymentID: "dep_1"})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if out.Reason != string(domain.ReasonImageRunsAsRoot) || out.Message != domain.ReasonImageRunsAsRoot.Message() {
			t.Errorf("failure = %q/%q", out.Reason, out.Message)
		}
	})

	t.Run("cancelled names what replaced it", func(t *testing.T) {
		dep := base
		dep.State = domain.StateCancelled
		s, _ := statusServer(&stubDeployments{
			rows: map[string]Deployment{"dep_1": dep},
			next: "dep_2",
		})
		_, out, err := s.status(t.Context(), account, statusInput{DeploymentID: "dep_1"})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if out.Reason != string(domain.ReasonSuperseded) || out.SupersededBy != "dep_2" {
			t.Errorf("cancellation = %q/%q", out.Reason, out.SupersededBy)
		}
	})

	t.Run("cancelled before its successor is visible", func(t *testing.T) {
		dep := base
		dep.State = domain.StateCancelled
		s, _ := statusServer(&stubDeployments{rows: map[string]Deployment{"dep_1": dep}})

		_, out, err := s.status(t.Context(), account, statusInput{DeploymentID: "dep_1"})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if out.SupersededBy != "" {
			t.Errorf("superseded_by = %q, want empty rather than an error", out.SupersededBy)
		}
	})
}

func TestStatusByNameReportsTheAppsLatestDeployment(t *testing.T) {
	// covers: AC-6
	t.Parallel()
	s, _ := statusServer(&stubDeployments{
		latest: Deployment{ID: "dep_7", AppID: "app_1", AccountID: "acc_1", State: domain.StateQueued},
	})
	_, out, err := s.status(t.Context(), auth.Account{ID: "acc_1"}, statusInput{Name: "hello"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if out.DeploymentID != "dep_7" {
		t.Errorf("deployment = %q, want dep_7", out.DeploymentID)
	}
}

func TestTheStatusDescriptionCarriesThePollingContract(t *testing.T) {
	// covers: AC-5, AC-7
	t.Parallel()
	for _, want := range []string{
		"deployment_id", "name", "exactly one",
		"queued", "healthy", "failed", "cancelled",
		"superseded_by", "few seconds", "minutes",
	} {
		if !strings.Contains(statusDescription, want) {
			t.Errorf("the description does not mention %q", want)
		}
	}
}

// covers spec 0009 AC-5: the engine that ran is reported from the moment the
// build starts, on a failed deployment as much as a healthy one, and omitted
// before there is one to report.
func TestStatusReportsTheBuildPathOnceThereIsOne(t *testing.T) {
	t.Parallel()
	account := auth.Account{ID: "acc_1"}

	for name, row := range map[string]struct {
		dep  Deployment
		want string
	}{
		"queued has no engine yet": {
			dep:  Deployment{ID: "dep_1", AppID: "app_1", AccountID: "acc_1", State: domain.StateQueued},
			want: "",
		},
		"building reports it": {
			dep:  Deployment{ID: "dep_1", AppID: "app_1", AccountID: "acc_1", State: domain.StateBuilding, BuildPath: "dockerfile"},
			want: "dockerfile",
		},
		"a failed build still reports it": {
			dep: Deployment{
				ID: "dep_1", AppID: "app_1", AccountID: "acc_1",
				State: domain.StateFailed, Reason: domain.ReasonBuildFailed, BuildPath: "dockerfile",
			},
			want: "dockerfile",
		},
		"a healthy buildpacks deploy reports it": {
			dep:  Deployment{ID: "dep_1", AppID: "app_1", AccountID: "acc_1", State: domain.StateHealthy, BuildPath: "buildpacks"},
			want: "buildpacks",
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := statusServer(&stubDeployments{rows: map[string]Deployment{"dep_1": row.dep}})

			_, out, err := s.status(t.Context(), account, statusInput{DeploymentID: "dep_1"})
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if out.BuildPath != row.want {
				t.Errorf("build_path = %q, want %q", out.BuildPath, row.want)
			}
		})
	}
}
