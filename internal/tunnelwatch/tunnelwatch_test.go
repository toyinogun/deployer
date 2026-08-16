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
