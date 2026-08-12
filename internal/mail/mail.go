// Package mail sends the platform's transactional messages through Resend. It is
// the only thing here that talks to an outside service, and it is deliberately
// thin: one POST, no SDK, no retry queue.
//
// Sending is best effort by design. A failure here never fails the request that
// triggered it: the account or the link is already committed, and the caller sees
// the same answer either way (spec 0007, AC-25).
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// sendURL is Resend's one endpoint this package uses.
const sendURL = "https://api.resend.com/emails"

// sendTimeout bounds one send. A slow provider must not hold a request open: the
// caller has already been answered by the time this matters, but the goroutine
// doing it still has to end.
const sendTimeout = 10 * time.Second

// Sender posts a message through Resend.
type Sender struct {
	apiKey string
	from   string
	client *http.Client
}

// Options configures a Sender.
type Options struct {
	// APIKey is DEPLOYER_RESEND_API_KEY. Never logged, at any level.
	APIKey string
	// From is DEPLOYER_MAIL_FROM, the address every message is sent as.
	From string
	// Client is the HTTP client to send through. Nil means one with a timeout.
	Client *http.Client
}

// New returns a sender, or nil when no API key is configured. A nil sender is a
// supported state: the endpoints that exist only to send mail answer
// mail_unavailable, and everything else, the whole MCP and upload path included,
// works normally (AC-26).
func New(opts Options) *Sender {
	if opts.APIKey == "" {
		return nil
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: sendTimeout}
	}
	return &Sender{apiKey: opts.APIKey, from: opts.From, client: opts.Client}
}

// Send posts one plain text message. The recipient address is in the request but
// never in an error: an error string is one more place a caller's address could
// end up in a log.
func (s *Sender) Send(ctx context.Context, to, subject, body string) error {
	payload, err := json.Marshal(map[string]any{
		"from":    s.from,
		"to":      []string{to},
		"subject": subject,
		"text":    body,
	})
	if err != nil {
		return fmt.Errorf("mail: encoding the message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sendURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("mail: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("mail: sending: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		// The status, and nothing from the body: a provider error can echo the
		// request back, and the request carries an address.
		return fmt.Errorf("mail: the provider answered %s", resp.Status)
	}
	return nil
}
