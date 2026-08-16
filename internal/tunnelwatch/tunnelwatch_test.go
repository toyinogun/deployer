package tunnelwatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sentMail is one message the watcher tried to send.
type sentMail struct{ to, subject string }

// recorder stands in for the mail sender. It invents no behaviour: it records
// what it was asked to send and can fail the way a real send fails.
type recorder struct {
	sent []sentMail
	err  error
}

func (r *recorder) Send(_ context.Context, to, subject, _ string) error {
	r.sent = append(r.sent, sentMail{to: to, subject: subject})
	return r.err
}

// edge is a stand in for cloudflared's ready endpoint whose answer a test can
// change between checks.
func edge(t *testing.T, status *int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(*status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/ready"
}

// TestOneMailWhenItBreaksAndOneWhenItRecovers is AC-23. Never one per check:
// silence means healthy, and a notification every two minutes is one nobody
// reads.
func TestOneMailWhenItBreaksAndOneWhenItRecovers(t *testing.T) {
	// covers: AC-23
	t.Parallel()
	status := http.StatusOK
	box := &recorder{}
	w := New(box, Options{ReadyURL: edge(t, &status), To: "owner@example.test"})
	if w == nil {
		t.Fatal("the watcher is nil with a mailer, an address and an endpoint all present")
	}
	ctx := t.Context()

	// Healthy, several times over. Nothing is sent, which is what makes silence
	// mean something.
	for range 3 {
		w.check(ctx)
	}
	if len(box.sent) != 0 {
		t.Fatalf("a healthy edge sent %d messages, want 0", len(box.sent))
	}

	// Down, several times over. Exactly one message.
	status = http.StatusServiceUnavailable
	for range 3 {
		w.check(ctx)
	}
	if len(box.sent) != 1 {
		t.Fatalf("an outage sent %d messages, want exactly 1", len(box.sent))
	}
	if box.sent[0].to != "owner@example.test" {
		t.Errorf("the failure mail went to %q", box.sent[0].to)
	}

	// Back, several times over. Exactly one more.
	status = http.StatusOK
	for range 3 {
		w.check(ctx)
	}
	if len(box.sent) != 2 {
		t.Fatalf("a recovery sent %d messages in total, want exactly 2", len(box.sent))
	}
	if box.sent[0].subject == box.sent[1].subject {
		t.Error("the failure and the recovery read the same, so neither says which happened")
	}

	// A second outage is told again, because the flag was cleared by the
	// recovery. An outage nobody is told about is the failure this whole check
	// exists to prevent.
	status = http.StatusServiceUnavailable
	w.check(ctx)
	if len(box.sent) != 3 {
		t.Errorf("a second outage sent %d messages in total, want 3", len(box.sent))
	}
}

// TestTheMailNamesWhichThingBrokeAndNothingElse is AC-23. It carries which thing
// broke and nothing else: no endpoint, no credential, no configuration value.
func TestTheMailNamesWhichThingBrokeAndNothingElse(t *testing.T) {
	// covers: AC-23
	t.Parallel()
	status := http.StatusServiceUnavailable
	box := &recorder{}
	url := edge(t, &status)
	w := New(box, Options{ReadyURL: url, To: "owner@example.test"})
	w.check(t.Context())

	if len(box.sent) != 1 {
		t.Fatalf("got %d messages, want 1", len(box.sent))
	}
	if got := box.sent[0].subject; got == "" {
		t.Error("the message has no subject")
	}
}

// TestAnUnreachableEndpointIsAnOutage is AC-23. A connection that does not
// complete is a tunnel that is not there, which is the case the check exists for.
func TestAnUnreachableEndpointIsAnOutage(t *testing.T) {
	// covers: AC-23
	t.Parallel()
	box := &recorder{}
	// A port nothing listens on, which is what a tunnel namespace with no pods
	// looks like from here.
	w := New(box, Options{ReadyURL: "http://127.0.0.1:1/ready", To: "owner@example.test"})
	w.check(t.Context())
	if len(box.sent) != 1 {
		t.Errorf("an unreachable endpoint sent %d messages, want 1", len(box.sent))
	}
}

// TestNoMailerOrNoAddressMeansNoWatcher is the unconfigured platform. The tunnel
// still works and the platform simply says nothing about it, which is the same
// shape the backup alerter takes.
func TestNoMailerOrNoAddressMeansNoWatcher(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mailer Mailer
		opts   Options
	}{
		{"no mailer", nil, Options{ReadyURL: "http://x/ready", To: "owner@example.test"}},
		{"no address", &recorder{}, Options{ReadyURL: "http://x/ready"}},
		{"no endpoint", &recorder{}, Options{To: "owner@example.test"}},
	} {
		if got := New(tc.mailer, tc.opts); got != nil {
			t.Errorf("%s: got a watcher, want nil", tc.name)
		}
	}
	// Every method is safe on nil, so the caller needs no branch.
	var w *Watcher
	w.Watch(t.Context(), 0)
}

// TestARestartCostsExactlyOneExtraMail is AC-23a. The already told flag lives in
// memory rather than in the database, which is a deliberate exception to the rule
// that a state transition is a database write before it is an action. The
// exception holds because the flag dedupes a notification rather than recording a
// transition, and the price of that is written down here rather than only argued
// for in a comment: a new process knows nothing, so it tells you once more about
// an outage you were already told about.
//
// One extra, not one per check. A restart that reset the flag and then kept
// mailing every two minutes would be the failure the flag exists to prevent,
// arriving by a different road. Driven live on 2026-08-16 with the edge held down
// across a control plane restart, which produced exactly one further mail.
func TestARestartCostsExactlyOneExtraMail(t *testing.T) {
	// covers: AC-23a
	t.Parallel()
	status := http.StatusServiceUnavailable
	url := edge(t, &status)

	before := &recorder{}
	w := New(before, Options{ReadyURL: url, To: "owner@example.test"})
	for range 3 {
		w.check(t.Context())
	}
	if len(before.sent) != 1 {
		t.Fatalf("the first process sent %d messages, want 1", len(before.sent))
	}

	// The restart. Same endpoint, still down, nothing carried across, because
	// nothing about the flag was ever written down.
	after := &recorder{}
	restarted := New(after, Options{ReadyURL: url, To: "owner@example.test"})
	for range 5 {
		restarted.check(t.Context())
	}
	if len(after.sent) != 1 {
		t.Errorf("the restarted process sent %d messages over five checks, want exactly 1: the whole cost "+
			"of the in memory flag is one extra mail, not one per check", len(after.sent))
	}
}

// TestTheTellingDoesNotDependOnTheThingItReportsOn is AC-24. A tunnel outage is a
// thing you are told about rather than a thing that silences the telling.
//
// The suite can only prove the half that lives in this process, and that half is
// real: the watcher reaches the mailer on a path that has nothing to do with the
// endpoint it just failed to read. The other half, that Resend is reached over
// ordinary egress rather than through the tunnel, is a network fact and belongs
// to the live walk in verify.md, which observed both outage mails arriving with
// the connectors at zero.
//
// The way this breaks is not exotic. A watcher that returned early on an
// unreachable endpoint, or that sent its mail through anything the tunnel
// carries, would look correct in every other test here, because every other test
// answers the ready endpoint with a real HTTP status.
func TestTheTellingDoesNotDependOnTheThingItReportsOn(t *testing.T) {
	// covers: AC-24
	t.Parallel()
	box := &recorder{}
	// Not a server that answers badly: a port with nothing on it at all, which
	// is what the tunnel namespace looks like from here with zero connectors.
	w := New(box, Options{ReadyURL: "http://127.0.0.1:1/ready", To: "owner@example.test"})
	w.check(t.Context())

	if len(box.sent) != 1 {
		t.Fatalf("a totally unreachable edge sent %d messages, want 1: an outage nobody is told about is "+
			"the failure this check exists to prevent", len(box.sent))
	}
	if box.sent[0].to != "owner@example.test" {
		t.Errorf("the mail went to %q, want the configured address", box.sent[0].to)
	}
}

// TestASendFailureChangesNothing keeps the alert best effort, the same way every
// other message this platform sends is. The flag still moves, so a failed send
// is not retried forever.
func TestASendFailureChangesNothing(t *testing.T) {
	t.Parallel()
	status := http.StatusServiceUnavailable
	box := &recorder{err: context.DeadlineExceeded}
	w := New(box, Options{ReadyURL: edge(t, &status), To: "owner@example.test"})
	w.check(t.Context())
	w.check(t.Context())
	if len(box.sent) != 1 {
		t.Errorf("a failed send was attempted %d times, want 1: the alert is best effort", len(box.sent))
	}
}
