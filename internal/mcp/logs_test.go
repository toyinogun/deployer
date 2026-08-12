package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/logs"
)

// stubPods stands in for the cluster. It records what it was asked for, which is
// how the tests prove the log API is not called at all in the empty case.
type stubPods struct {
	pods    []logs.PodStatus
	current string
	prev    string
	listErr error
	logErr  error

	calls     []string
	lastTail  int
	lastNS    string
	lastSlug  string
	lastPod   string
	logCalled int
}

func (p *stubPods) PodsForApp(_ context.Context, namespace, slug string) ([]logs.PodStatus, error) {
	p.lastNS, p.lastSlug = namespace, slug
	if p.listErr != nil {
		return nil, p.listErr
	}
	return p.pods, nil
}

func (p *stubPods) PodLog(_ context.Context, namespace, pod string, tail int, previous bool) (string, error) {
	p.logCalled++
	p.lastNS, p.lastPod, p.lastTail = namespace, pod, tail
	p.calls = append(p.calls, fmt.Sprintf("previous=%v tail=%d", previous, tail))
	if p.logErr != nil {
		return "", p.logErr
	}
	if previous {
		return p.prev, nil
	}
	return p.current, nil
}

// appUnknown is the one answer every refused log read gets.
var appUnknown = string(domain.ReasonAppUnknown) + ": " + domain.ReasonAppUnknown.Message()

// internalOnly is what a fault reads as, which must never be appUnknown.
var internalOnly = string(domain.ReasonInternal) + ": " + domain.ReasonInternal.Message()

// logsServer builds a tool surface over one app called hello and a stub cluster.
func logsServer(pods Pods, deployments *stubDeployments) (*Server, *silentAuditor) {
	apps := &stubApps{existing: map[string]App{
		"hello": {ID: "app_1", Slug: "hello-a1b2c3", Name: "hello"},
	}}
	auditor := &silentAuditor{}
	return &Server{
		auditor:     auditor,
		apps:        apps,
		deployments: deployments,
		uploads:     stubUploads{},
		pods:        pods,
		opts: Options{
			PublicURL:      "https://deployer.example.org",
			AppDomain:      "deploy.example.org",
			SecretLiterals: []string{"s3cr3t-registry-pass"},
		},
	}, auditor
}

// running is a started, ready pod with no restarts.
func running(name string) logs.PodStatus {
	return logs.PodStatus{Name: name, Ready: true, ContainerStarted: true}
}

// line stamps one message the way the kubelet does.
func line(n int, message string) string {
	return fmt.Sprintf("2026-08-12T10:00:%02d.000000000Z %s\n", n%60, message)
}

func TestLogsReturnsTheAppsRecentOutput(t *testing.T) {
	// covers: AC-1, AC-2
	t.Parallel()
	pods := &stubPods{
		pods:    []logs.PodStatus{running("hello-abc")},
		current: line(0, "listening on :8080") + line(1, "served GET /"),
	}
	s, auditor := logsServer(pods, &stubDeployments{})

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if out.AppName != "hello" || len(out.Entries) != 2 {
		t.Fatalf("out = %+v", out)
	}
	if out.Entries[0].Message != "listening on :8080" || out.Entries[1].Message != "served GET /" {
		t.Fatalf("entries are not oldest to newest: %+v", out.Entries)
	}
	if out.Entries[0].At == "" {
		t.Error("the kubelet's timestamp was not carried through")
	}
	// The default applies, nothing was clamped, and no previous block exists.
	if out.TailLines != logs.DefaultTail || out.Clamped || out.Previous != nil {
		t.Errorf("bounds reported wrong: tail=%d clamped=%v previous=%v", out.TailLines, out.Clamped, out.Previous)
	}
	// A successful read is not an access decision, so nothing is audited (AC-9).
	if len(auditor.rows) != 0 {
		t.Errorf("a successful read audited %+v", auditor.rows)
	}
}

func TestLogsReadsOnlyTheAppsOwnNamespace(t *testing.T) {
	// covers: AC-11. The namespace and the slug are derived from the resolved
	// app, never from anything the caller sent.
	t.Parallel()
	pods := &stubPods{pods: []logs.PodStatus{running("hello-abc")}, current: line(0, "hi")}
	s, _ := logsServer(pods, &stubDeployments{})

	if _, _, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"}); err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if pods.lastNS != "app-hello-a1b2c3" || pods.lastSlug != "hello-a1b2c3" {
		t.Fatalf("read %s / %s, want the app's own namespace and slug", pods.lastNS, pods.lastSlug)
	}
}

func TestLogsClampsAnOversizeTail(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	tests := map[string]struct {
		asked   int
		applied int
		clamped bool
	}{
		"absent":       {0, logs.DefaultTail, false},
		"negative":     {-3, logs.DefaultTail, false},
		"in range":     {25, 25, false},
		"over the cap": {9000, logs.MaxTail, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			pods := &stubPods{pods: []logs.PodStatus{running("hello-abc")}, current: line(0, "hi")}
			s, _ := logsServer(pods, &stubDeployments{})

			_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"},
				logsInput{Name: "hello", TailLines: tc.asked})
			if err != nil {
				t.Fatalf("getLogs: %v", err)
			}
			if out.TailLines != tc.applied || out.Clamped != tc.clamped {
				t.Fatalf("out tail=%d clamped=%v, want %d and %v",
					out.TailLines, out.Clamped, tc.applied, tc.clamped)
			}
			// The value that was applied is the one asked of the cluster.
			if pods.lastTail != tc.applied {
				t.Errorf("asked the cluster for %d lines, want %d", pods.lastTail, tc.applied)
			}
		})
	}
}

func TestLogsTruncatesTheOldestAndSaysSo(t *testing.T) {
	// covers: AC-3
	t.Parallel()
	// Far past the byte ceiling, so bounding bites regardless of the tail.
	var raw strings.Builder
	for i := 0; i < 400; i++ {
		raw.WriteString(line(i, fmt.Sprintf("%04d ", i)+strings.Repeat("x", 400)))
	}
	pods := &stubPods{pods: []logs.PodStatus{running("hello-abc")}, current: raw.String()}
	s, _ := logsServer(pods, &stubDeployments{})

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"},
		logsInput{Name: "hello", TailLines: logs.MaxTail})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if !out.Truncated || out.Dropped == 0 {
		t.Fatalf("a block over the ceiling was not reported truncated: %+v", out)
	}
	if len(out.Entries)+out.Dropped != 400 {
		t.Errorf("kept %d and dropped %d, which does not account for 400", len(out.Entries), out.Dropped)
	}
	// The newest survived and the oldest went.
	if !strings.HasPrefix(out.Entries[len(out.Entries)-1].Message, "0399") {
		t.Errorf("the newest entry was dropped: %q", out.Entries[len(out.Entries)-1].Message)
	}
	if strings.HasPrefix(out.Entries[0].Message, "0000") {
		t.Error("the oldest entry survived, so truncation ran from the wrong end")
	}
	size := 0
	for _, e := range out.Entries {
		size += len(e.At) + len(e.Message)
	}
	if size > logs.CurrentBytes {
		t.Errorf("the block is %d bytes, over the %d ceiling", size, logs.CurrentBytes)
	}
}

func TestLogsCarriesThePreviousContainerAfterARestart(t *testing.T) {
	// covers: AC-4. A noisy current container must not shrink the crash block.
	t.Parallel()
	var noisy strings.Builder
	for i := 0; i < 400; i++ {
		noisy.WriteString(line(i, strings.Repeat("y", 400)))
	}
	prev := line(0, "panic: nil map write") + line(1, "goroutine 1 [running]:")
	pods := &stubPods{
		pods:    []logs.PodStatus{{Name: "hello-abc", Ready: true, ContainerStarted: true, RestartCount: 2}},
		current: noisy.String(),
		prev:    prev,
	}
	s, _ := logsServer(pods, &stubDeployments{})

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if len(out.Previous) != 2 || out.Previous[0].Message != "panic: nil map write" {
		t.Fatalf("previous = %+v", out.Previous)
	}
	if !out.Truncated {
		t.Error("the noisy current block should still report truncation")
	}
	if len(pods.calls) != 2 || pods.calls[1] != fmt.Sprintf("previous=true tail=%d", logs.PreviousLines) {
		t.Errorf("the previous read was not made with its own cap: %v", pods.calls)
	}
}

func TestLogsOmitsThePreviousBlockWithNoRestart(t *testing.T) {
	// covers: AC-4. Absent, not present and empty, so an agent can tell "no
	// previous container" from "one that printed nothing".
	t.Parallel()
	pods := &stubPods{pods: []logs.PodStatus{running("hello-abc")}, current: line(0, "hi")}
	s, _ := logsServer(pods, &stubDeployments{})

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if out.Previous != nil {
		t.Fatalf("previous = %+v, want absent", out.Previous)
	}
	if pods.logCalled != 1 {
		t.Errorf("the log API was called %d times, want 1", pods.logCalled)
	}
}

func TestLogsReadsTheNewestPodAndWarnsWhenAnOlderMayServe(t *testing.T) {
	// covers: AC-5
	t.Parallel()
	pods := &stubPods{
		pods: []logs.PodStatus{
			{Name: "hello-new", Ready: false, ContainerStarted: true, RestartCount: 1},
			{Name: "hello-old", Ready: true, ContainerStarted: true},
		},
		current: line(0, "starting"),
		prev:    line(0, "crashed"),
	}
	s, _ := logsServer(pods, &stubDeployments{})

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	if pods.lastPod != "hello-new" {
		t.Fatalf("read pod %q, want the newest", pods.lastPod)
	}
	if out.Note != noteOlderPod {
		t.Fatalf("note = %q, want the older pod warning", out.Note)
	}
}

func TestLogsRedactsBothBlocks(t *testing.T) {
	// covers: AC-6
	t.Parallel()
	pods := &stubPods{
		pods: []logs.PodStatus{{Name: "hello-abc", Ready: true, ContainerStarted: true, RestartCount: 1}},
		current: line(0, "Authorization: Bearer abcdef0123456789abcdef") +
			line(1, "dialing postgres://admin:hunter2@db:5432/app") +
			line(2, "pull secret s3cr3t-registry-pass in use") +
			line(3, strings.Repeat("z", 200)),
		prev: line(0, "API_TOKEN=tok_live_abcdef123456"),
	}
	s, _ := logsServer(pods, &stubDeployments{})

	_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	all := fmt.Sprintf("%v %v", out.Entries, out.Previous)
	for _, secret := range []string{
		"abcdef0123456789abcdef", "hunter2", "s3cr3t-registry-pass", "tok_live_abcdef123456",
	} {
		if strings.Contains(all, secret) {
			t.Errorf("%q reached the caller", secret)
		}
	}
	// A line that is merely long is not a secret, and blanking it would make the
	// tool useless for the debugging it exists for.
	if !strings.Contains(all, strings.Repeat("z", 200)) {
		t.Error("a long but ordinary line was blanked")
	}
}

func TestLogsEmptyCaseIsASuccess(t *testing.T) {
	// covers: AC-7, AC-10. Decided from pod status, before the log API is called
	// at all, so a container that has not started is never reported as a fault.
	t.Parallel()
	tests := map[string]struct {
		pods  []logs.PodStatus
		state domain.State
		note  string
	}{
		"no pod at all": {
			pods: nil, state: domain.StateBuilding, note: noteNotStarted,
		},
		"a container still waiting": {
			pods:  []logs.PodStatus{{Name: "hello-abc", ContainerStarted: false}},
			state: domain.StateDeploying, note: noteNotStarted,
		},
		"healthy with no pod left": {
			pods: nil, state: domain.StateHealthy, note: noteGone,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			pods := &stubPods{pods: tc.pods, logErr: errors.New("the log API must not be called")}
			deployments := &stubDeployments{latest: Deployment{ID: "dep_1", AppID: "app_1", State: tc.state}}
			s, auditor := logsServer(pods, deployments)

			_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
			if err != nil {
				t.Fatalf("the empty case reported a failure: %v", err)
			}
			if pods.logCalled != 0 {
				t.Error("the log API was called before the pod status gate decided")
			}
			if len(out.Entries) != 0 {
				t.Errorf("entries = %+v, want none", out.Entries)
			}
			if out.State != string(tc.state) || out.Note != tc.note {
				t.Errorf("state=%q note=%q, want %q and %q", out.State, out.Note, tc.state, tc.note)
			}
			if len(auditor.rows) != 0 {
				t.Errorf("the empty case audited %+v", auditor.rows)
			}
		})
	}
}

func TestLogsAnswersUnknownAndAnotherAccountIdentically(t *testing.T) {
	// covers: AC-8, AC-9
	t.Parallel()
	for name, in := range map[string]logsInput{
		"a name that does not exist": {Name: "nothing-here"},
		"no name at all":             {},
	} {
		t.Run(name, func(t *testing.T) {
			// ByName is account scoped in the store, so another account's app
			// arrives here exactly as a missing row does.
			pods := &stubPods{logErr: errors.New("must not be reached")}
			s, auditor := logsServer(pods, &stubDeployments{})

			_, _, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, in)
			if err == nil || err.Error() != appUnknown {
				t.Fatalf("error = %v, want %q", err, appUnknown)
			}
			if len(auditor.rows) != 1 {
				t.Fatalf("audit = %+v, want exactly one refusal", auditor.rows)
			}
			row := auditor.rows[0]
			if row.Action != auth.ActionLogs || row.AccountID != "acc_1" || row.Allowed {
				t.Errorf("audit row = %+v, want a logs denial for acc_1", row)
			}
			if pods.logCalled != 0 {
				t.Error("a refused read still touched the cluster")
			}
		})
	}
}

func TestLogsReportsAFaultAsInternalWithNoAuditRow(t *testing.T) {
	// covers: AC-9, AC-10. A fault is not an access decision: answering
	// app_unknown would tell the agent its own app name is wrong, and the audit
	// row would record a denial that never happened.
	t.Parallel()
	tests := map[string]func() (*stubPods, *stubApps){
		"the store cannot be read": func() (*stubPods, *stubApps) {
			return &stubPods{}, &stubApps{readErr: errors.New("database is locked")}
		},
		"the pod list fails": func() (*stubPods, *stubApps) {
			return &stubPods{listErr: errors.New("connection refused")}, nil
		},
		"the log read fails midway": func() (*stubPods, *stubApps) {
			return &stubPods{
				pods:   []logs.PodStatus{running("hello-abc")},
				logErr: errors.New("connection reset"),
			}, nil
		},
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			pods, badApps := build()
			s, auditor := logsServer(pods, &stubDeployments{})
			if badApps != nil {
				s.apps = badApps
			}

			_, out, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
			if err == nil || err.Error() != internalOnly {
				t.Fatalf("error = %v, want %q", err, internalOnly)
			}
			// A partial read is never presented as the app's output.
			if len(out.Entries) != 0 {
				t.Errorf("a failed read still returned %d entries", len(out.Entries))
			}
			if len(auditor.rows) != 0 {
				t.Errorf("a fault was audited as a refusal: %+v", auditor.rows)
			}
		})
	}
}

func TestLogsWithNoClusterFailsRatherThanReportingSilence(t *testing.T) {
	// covers: AC-10. A local run with no cluster must not answer "your app
	// printed nothing", which is a claim it cannot make.
	t.Parallel()
	s, auditor := logsServer(nil, &stubDeployments{})

	_, _, err := s.getLogs(t.Context(), auth.Account{ID: "acc_1"}, logsInput{Name: "hello"})
	if err == nil || err.Error() != internalOnly {
		t.Fatalf("error = %v, want %q", err, internalOnly)
	}
	if len(auditor.rows) != 0 {
		t.Errorf("audit = %+v, want none", auditor.rows)
	}
}

func TestTheToolDescriptionCarriesItsContract(t *testing.T) {
	// covers: AC-13. Nothing else tests this drift, so it is pinned here.
	t.Parallel()
	for _, want := range []string{
		"snapshot", "not a stream", "200", "1000", "clamped",
		"oldest lines are dropped", "previous", "best effort",
	} {
		if !strings.Contains(logsDescription, want) {
			t.Errorf("the description does not mention %q", want)
		}
	}
	if strings.Contains(logsDescription, "guarantee") &&
		!strings.Contains(logsDescription, "not a guarantee") {
		t.Error("the description implies redaction is a guarantee")
	}
}
