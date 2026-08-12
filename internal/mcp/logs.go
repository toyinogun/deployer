package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/deploy"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/logs"
)

// logsDescription is contract rather than decoration: it is where an agent
// learns that this is a snapshot and not a stream, what it gets by default and
// at most, which end is dropped when the answer is too big, that a crash puts a
// second block in the response, and that redaction is best effort rather than a
// guarantee (spec 0006, AC-13).
const logsDescription = `Read an app's recent output, so a misbehaving app can be debugged without a terminal.

Give the app's name. This is a snapshot, not a stream: each call returns the
newest lines that are still on the node, and there is no follow, no search,
and no time window.

tail_lines is optional. It defaults to 200 and is capped at 1000; a larger ask
is clamped rather than refused, and the response echoes the value applied. The
answer is also capped by size, and when that cap is hit the oldest lines are dropped,
never the newest, with truncated and dropped saying so.

If the app has crashed and restarted, the previous field carries a smaller,
separately capped block of the crashed container's output, which is usually
where the cause is. It is absent when there has been no restart.

An app that has not started a container yet is a success with no entries, plus
the deployment's state and one sentence saying why there is nothing to show.

Secrets are blanked on a best effort basis: bearer and basic credentials, JWTs,
passwords inside URLs, access key ids, and assignments named like a key, token,
secret, or password. This is pattern matching, not a guarantee. A secret your
app prints in an unusual shape can still appear here.

Only your own app's output is returned. Build output, platform logs, and
anything outside the app are never reachable through this.`

// logsInput is the whole argument surface. The namespace and the container are
// derived from the resolved app, never named by a caller (AC-11).
type logsInput struct {
	Name      string `json:"name" jsonschema:"the app's name, the same one deploy_app was given"`
	TailLines int    `json:"tail_lines,omitempty" jsonschema:"how many of the newest lines to return; defaults to 200, capped at 1000"`
}

// logsOutput omits optional fields rather than returning them null or empty,
// matching deployment_status, so an agent can tell "no previous container" from
// "a previous container that printed nothing" (spec 0006, API surface).
type logsOutput struct {
	AppName   string       `json:"app_name"`
	State     string       `json:"state,omitempty"`
	TailLines int          `json:"tail_lines"`
	Entries   []logs.Entry `json:"entries"`
	Previous  []logs.Entry `json:"previous,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	Dropped   int          `json:"dropped,omitempty"`
	Clamped   bool         `json:"clamped,omitempty"`
	Note      string       `json:"note,omitempty"`
}

// The sentences the empty case carries. They are the whole of what a caller is
// told about why there is nothing, so each says what is true rather than
// implying a fault (AC-7).
const (
	noteNotStarted = "no container has started yet, so there is nothing to show"
	noteGone       = "the app has no pod right now, so its earlier output is no longer available"
	noteOlderPod   = "the newest pod is not ready, so an older pod may still be serving and this output may not be what a request reaches"
)

// getLogs reads the app's own recent output. It writes an audit row only when it
// refuses: a successful read is not an access decision, and neither is a fault
// (AC-9).
func (s *Server) getLogs(ctx context.Context, account auth.Account, in logsInput) (*mcp.CallToolResult, logsOutput, error) {
	if in.Name == "" {
		return nil, logsOutput{}, s.denyLogs(ctx, account.ID, errors.New("name is required"))
	}

	// Scoped before anything is read: another account's app and one that does not
	// exist get the same answer, so the tool cannot be used to learn which app
	// names exist (AC-8).
	app, err := s.apps.ByName(ctx, account.ID, in.Name)
	if errors.Is(err, ErrNoApp) {
		return nil, logsOutput{}, s.denyLogs(ctx, account.ID, err)
	}
	if err != nil {
		// A fault is not a refusal, so it is not audited and does not answer
		// app_unknown, which would tell the agent its own name is wrong.
		return nil, logsOutput{}, toolError(auth.ActionLogs, domain.ReasonInternal,
			fmt.Errorf("resolving app %q: %w", in.Name, err))
	}

	out, err := s.readLogs(ctx, app, in.TailLines)
	if err != nil {
		return nil, logsOutput{}, toolError(auth.ActionLogs, domain.ReasonInternal, err)
	}
	return nil, out, nil
}

// readLogs is the read itself, once the app is resolved and the caller's right
// to it is settled. It returns a complete bounded answer or an error: there is
// no partial success (spec 0006, Key invariants).
func (s *Server) readLogs(ctx context.Context, app App, requested int) (logsOutput, error) {
	tail, clamped := logs.ClampTail(requested)
	out := logsOutput{
		AppName:   app.Name,
		TailLines: tail,
		Clamped:   clamped,
		Entries:   []logs.Entry{},
	}

	if s.pods == nil {
		return logsOutput{}, errors.New("no cluster access, so no log can be read")
	}
	namespace := deploy.NamespaceName(app.Slug)
	pods, err := s.pods.PodsForApp(ctx, namespace, app.Slug)
	// A namespace the platform cannot read yet is the app not having started,
	// which is the empty case rather than a fault: it is what every read during a
	// build sees, because the namespace is created at the deploy step (AC-7).
	if errors.Is(err, logs.ErrNoNamespace) {
		out.State, out.Note = s.emptyCase(ctx, app, true)
		return out, nil
	}
	if err != nil {
		return logsOutput{}, fmt.Errorf("listing the pods of app %s: %w", app.ID, err)
	}

	// The gate: the empty case is decided from pod status, before the log API is
	// called at all, so a container that has not started is never reported as a
	// fault and a fault is never reported as an empty log (AC-7, AC-10).
	if len(pods) == 0 || !pods[0].ContainerStarted {
		out.State, out.Note = s.emptyCase(ctx, app, len(pods) == 0)
		return out, nil
	}

	pod := pods[0]
	raw, err := s.pods.PodLog(ctx, namespace, pod.Name, tail, false)
	if err != nil {
		return logsOutput{}, fmt.Errorf("reading the log of pod %s/%s: %w", namespace, pod.Name, err)
	}
	// Redaction runs on every line before bounding, so the reported sizes and
	// counts describe what the caller actually receives (AC-3, AC-6).
	entries, dropped := logs.Bound(
		logs.RedactAll(logs.Parse(raw), s.opts.SecretLiterals), tail, logs.CurrentBytes)
	out.Entries, out.Dropped, out.Truncated = entries, dropped, dropped > 0

	// The previous block is capped independently, so a noisy current container
	// can never squeeze the crash that caused the restart out of the answer
	// (AC-4).
	if pod.RestartCount > 0 {
		prev, err := s.pods.PodLog(ctx, namespace, pod.Name, logs.PreviousLines, true)
		if err != nil {
			return logsOutput{}, fmt.Errorf("reading the previous container of pod %s/%s: %w", namespace, pod.Name, err)
		}
		out.Previous, _ = logs.Bound(
			logs.RedactAll(logs.Parse(prev), s.opts.SecretLiterals), logs.PreviousLines, logs.PreviousBytes)
	}

	if !pod.Ready && len(pods) > 1 {
		out.Note = noteOlderPod
	}
	return out, nil
}

// emptyCase reports the deployment's current state and the one sentence saying
// why there is nothing to show. A state that cannot be read is left empty rather
// than turned into a failure: the answer is still true without it.
func (s *Server) emptyCase(ctx context.Context, app App, noPod bool) (state, note string) {
	dep, err := s.deployments.LatestForApp(ctx, app.ID)
	if err != nil {
		return "", noteNotStarted
	}
	// A healthy app with no pod has run and lost its output, which is a different
	// thing from an app that has never started (AC-7).
	if noPod && dep.State == domain.StateHealthy {
		return string(dep.State), noteGone
	}
	return string(dep.State), noteNotStarted
}

// denyLogs records the refusal and gives back the one answer every refused log
// read gets, whatever it was that did not resolve (AC-8, AC-9).
func (s *Server) denyLogs(ctx context.Context, accountID string, cause error) error {
	auth.Record(ctx, s.auditor, auth.Audit{
		AccountID: accountID,
		Action:    auth.ActionLogs,
		Reason:    string(domain.ReasonAppUnknown),
	})
	return toolError(auth.ActionLogs, domain.ReasonAppUnknown, cause)
}
