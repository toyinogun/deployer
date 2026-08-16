// Package tunnelwatch tells the platform's owner when the public edge has no
// connectors, and once more when it comes back.
//
// It reads cloudflared's own ready endpoint over ordinary HTTP through a Service
// in the tunnel namespace, so it needs no Kubernetes API read and no new Role or
// RoleBinding anywhere: this feature grants the platform no new cluster rights at
// all (spec 0021, AC-23).
//
// The mail leaves the cluster over the platform's ordinary outbound path and does
// not depend on the tunnel, so a tunnel outage is a thing you are told about
// rather than a thing that silences the telling (AC-24).
package tunnelwatch

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// checkTimeout bounds one read of the ready endpoint. It is a pod to pod hop, so
// anything slower than this is a tunnel that is not answering rather than one
// that is busy.
const checkTimeout = 5 * time.Second

// DefaultInterval is how often the edge is checked. Often enough that an outage
// is noticed within a few minutes, rarely enough that it is not itself traffic.
const DefaultInterval = 2 * time.Minute

// Mailer is the one thing this package needs from the mail package, declared
// here where it is used, exactly as internal/backup declares its own.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Watcher polls the tunnel's ready endpoint and reports the transitions.
type Watcher struct {
	readyURL string
	mailer   Mailer
	to       string
	client   *http.Client

	// told is whether the owner has already been told the edge is down.
	//
	// In memory, not in the database, and that is a deliberate exception to the
	// rule that a state transition is a database write before it is an action.
	// It holds because this flag dedupes a notification rather than recording a
	// transition: nothing reads it, nothing branches on it beyond the send below,
	// and the worst a pod restart costs is one extra mail (AC-23a).
	//
	// The watcher runs on one goroutine, so this needs no lock.
	told bool
}

// Options configures a Watcher.
type Options struct {
	// ReadyURL is cloudflared's ready endpoint, reached through the Service in
	// the tunnel namespace on cluster DNS.
	ReadyURL string
	// To is where a failure reports. Empty means no watcher.
	To string
	// Client is the HTTP client to check through. Nil means one with a timeout.
	Client *http.Client
}

// New returns a watcher, or nil when there is no mailer, nowhere to send, or no
// endpoint to read. A nil watcher is a supported state: the tunnel still works
// and the platform simply says nothing about it, which is the same shape the
// backup alerter already takes.
func New(mailer Mailer, opts Options) *Watcher {
	if mailer == nil || opts.To == "" || opts.ReadyURL == "" {
		return nil
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: checkTimeout}
	}
	return &Watcher{readyURL: opts.ReadyURL, mailer: mailer, to: opts.To, client: opts.Client}
}

// Watch checks the edge every interval until ctx is done. It returns rather than
// panicking on a nil receiver, so the caller needs no branch.
func (w *Watcher) Watch(ctx context.Context, interval time.Duration) {
	if w == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	slog.InfoContext(ctx, "tunnel health check started", "interval", interval, "endpoint", w.readyURL)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.check(ctx)
		}
	}
}

// check reads the endpoint once and sends at most one message.
//
// One mail when it breaks and one when it recovers, never one per check, which
// is the same discipline the backup alerter holds: silence means healthy, and a
// notification every two minutes is a notification nobody reads.
func (w *Watcher) check(ctx context.Context) {
	err := w.ready(ctx)
	switch {
	case err != nil && !w.told:
		w.told = true
		slog.ErrorContext(ctx, "the public edge has no ready connectors", "error", err)
		w.send(ctx, "Deployer: the public edge is down",
			"The Cloudflare tunnel has no ready connectors, so the console and every deployed app "+
				"are unreachable from the internet.\n\nThe tailnet is unaffected.\n")
	case err == nil && w.told:
		w.told = false
		slog.InfoContext(ctx, "the public edge has ready connectors again")
		w.send(ctx, "Deployer: the public edge is back",
			"The Cloudflare tunnel has ready connectors again.\n")
	case err != nil:
		// Still down, already said so. Logged at debug so a long outage does not
		// fill the log with one line every two minutes.
		slog.DebugContext(ctx, "the public edge is still down", "error", err)
	}
}

// ready reads cloudflared's ready endpoint. Anything other than a 200 is down:
// the endpoint reports whether this connector has registered with Cloudflare, so
// a body that is not a success is a connector that has not.
func (w *Watcher) ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.readyURL, nil)
	if err != nil {
		return fmt.Errorf("tunnelwatch: building the ready request: %w", err)
	}
	res, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("tunnelwatch: reading the ready endpoint: %w", err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			slog.WarnContext(ctx, "closing the tunnel ready response failed", "error", err)
		}
	}()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("tunnelwatch: the ready endpoint answered %d", res.StatusCode)
	}
	return nil
}

// send is best effort, the same way every other message this platform sends is.
// A failure here is logged and changes nothing. The recipient never enters the
// log, and neither does anything about the tunnel beyond which thing broke.
func (w *Watcher) send(ctx context.Context, subject, body string) {
	if err := w.mailer.Send(ctx, w.to, subject, body); err != nil {
		slog.ErrorContext(ctx, "could not send the tunnel alert", "error", err)
	}
}
