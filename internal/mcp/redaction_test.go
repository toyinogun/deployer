package mcp

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/logs"
)

// appsOf reaches the stub behind a tool surface, so a test can say what the app
// has configured and what its running release was deployed with.
func appsOf(t *testing.T, s *Server) *stubApps {
	t.Helper()
	apps, ok := s.apps.(*stubApps)
	if !ok {
		t.Fatalf("the server is not backed by the stub: %T", s.apps)
	}
	return apps
}

func TestAnAppThatPrintsItsOwnSecretHasThatLineBlanked(t *testing.T) {
	// covers: spec 0010 AC-11
	t.Parallel()
	pods := &stubPods{
		pods:    []logs.PodStatus{running("hello-abc")},
		current: line(0, "connecting with postgres://user:supersecretvalue@db/app"),
	}
	s, _ := logsServer(pods, &stubDeployments{})
	apps := appsOf(t, s)
	if err := apps.SetConfig(t.Context(), "app_1", []ConfigEntry{
		{Key: "DATABASE_URL", Value: "supersecretvalue", Secret: true},
	}); err != nil {
		t.Fatalf("setting the configuration: %v", err)
	}

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if strings.Contains(out.Entries[0].Message, "supersecretvalue") {
		t.Errorf("the secret survived redaction: %q", out.Entries[0].Message)
	}
}

func TestAValueThatIsNotSecretIsLeftAlone(t *testing.T) {
	// covers: spec 0010 AC-11
	t.Parallel()
	pods := &stubPods{
		pods:    []logs.PodStatus{running("hello-abc")},
		current: line(0, "log level is verbosedebug"),
	}
	s, _ := logsServer(pods, &stubDeployments{})
	if err := appsOf(t, s).SetConfig(t.Context(), "app_1", []ConfigEntry{
		{Key: "LOG_LEVEL", Value: "verbosedebug"},
	}); err != nil {
		t.Fatalf("setting the configuration: %v", err)
	}

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if !strings.Contains(out.Entries[0].Message, "verbosedebug") {
		t.Errorf("a value nobody called secret was blanked: %q", out.Entries[0].Message)
	}
}

func TestAShortSecretIsNotLiteralMatched(t *testing.T) {
	// covers: spec 0010 AC-11
	t.Parallel()
	// Blanking every "dev" in the output destroys the log without protecting
	// anything worth protecting, which the spec accepts as a real hole.
	pods := &stubPods{
		pods:    []logs.PodStatus{running("hello-abc")},
		current: line(0, "running in dev against the dev cluster"),
	}
	s, _ := logsServer(pods, &stubDeployments{})
	if err := appsOf(t, s).SetConfig(t.Context(), "app_1", []ConfigEntry{
		{Key: "MODE", Value: "dev", Secret: true},
	}); err != nil {
		t.Fatalf("setting the configuration: %v", err)
	}

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if !strings.Contains(out.Entries[0].Message, "dev cluster") {
		t.Errorf("a three letter value was literal matched and wrecked the line: %q", out.Entries[0].Message)
	}
}

func TestARotatedSecretIsStillBlankedFromTheRunningPod(t *testing.T) {
	// covers: spec 0010 AC-11
	t.Parallel()
	// The pod was deployed with the old value and printed it. The key has since
	// been set again, so the old value survives only in the release snapshot.
	pods := &stubPods{
		pods:    []logs.PodStatus{running("hello-abc")},
		current: line(0, "authenticated with oldsecretvalue"),
	}
	s, _ := logsServer(pods, &stubDeployments{})
	apps := appsOf(t, s)
	apps.released = map[string]string{"API_KEY": "oldsecretvalue"}
	if err := apps.SetConfig(t.Context(), "app_1", []ConfigEntry{
		{Key: "API_KEY", Value: "newsecretvalue", Secret: true},
	}); err != nil {
		t.Fatalf("rotating the secret: %v", err)
	}

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if strings.Contains(out.Entries[0].Message, "oldsecretvalue") {
		t.Errorf("the rotated secret survived redaction: %q", out.Entries[0].Message)
	}
}

func TestAReleasedValueWhoseKeyIsNoLongerSecretIsNotBlanked(t *testing.T) {
	// covers: spec 0010 AC-11
	t.Parallel()
	// The accepted hole, written down so it is a decision rather than a surprise:
	// secrecy is read from the key as it stands today, not per release.
	pods := &stubPods{
		pods:    []logs.PodStatus{running("hello-abc")},
		current: line(0, "started with wasonceasecret"),
	}
	s, _ := logsServer(pods, &stubDeployments{})
	apps := appsOf(t, s)
	apps.released = map[string]string{"SETTING": "wasonceasecret"}
	if err := apps.SetConfig(t.Context(), "app_1", []ConfigEntry{
		{Key: "SETTING", Value: "plainnow"},
	}); err != nil {
		t.Fatalf("setting the configuration: %v", err)
	}

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if !strings.Contains(out.Entries[0].Message, "wasonceasecret") {
		t.Errorf("the value was blanked, so this test no longer describes the behaviour: %q", out.Entries[0].Message)
	}
}

func TestThePlatformsOwnSecretIsStillBlanked(t *testing.T) {
	// covers: spec 0006 AC-6
	t.Parallel()
	pods := &stubPods{
		pods:    []logs.PodStatus{running("hello-abc")},
		current: line(0, "pulling with s3cr3t-registry-pass"),
	}
	s, _ := logsServer(pods, &stubDeployments{})

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if strings.Contains(out.Entries[0].Message, "s3cr3t-registry-pass") {
		t.Errorf("the registry credential survived redaction: %q", out.Entries[0].Message)
	}
}
