package mail_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/mail"
)

// roundTrip is an http.RoundTripper built from a function, so a test owns what
// the provider answers without standing up a server.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// clientAnswering returns a client whose every request is handed to fn.
func clientAnswering(fn roundTrip) *http.Client { return &http.Client{Transport: fn} }

// reply builds a canned provider response.
func reply(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
}

// TestNewWithoutAKeyIsNilRatherThanASenderThatFails is AC-26: no key configured
// is a supported state, and identity.Service reads a nil Mailer as the reason to
// answer mail_unavailable rather than to fail a send.
//
// covers: AC-26
func TestNewWithoutAKeyIsNilRatherThanASenderThatFails(t *testing.T) {
	if s := mail.New(mail.Options{From: "deployer@example.org"}); s != nil {
		t.Error("an empty API key returned a sender, so nothing downstream can tell mail is unconfigured")
	}
	if s := mail.New(mail.Options{APIKey: "re_test", From: "deployer@example.org"}); s == nil {
		t.Error("a configured key returned no sender")
	}
}

// TestSendPostsTheMessageResendExpects pins the request shape: the endpoint, the
// bearer key, and a body carrying the configured From, the single recipient, and
// the plain text.
func TestSendPostsTheMessageResendExpects(t *testing.T) {
	var got *http.Request
	var body []byte

	s := mail.New(mail.Options{
		APIKey: "re_secret_key",
		From:   "deployer@example.org",
		Client: clientAnswering(func(r *http.Request) (*http.Response, error) {
			got = r
			body, _ = io.ReadAll(r.Body)
			return reply(http.StatusOK, `{"id":"abc"}`), nil
		}),
	})

	if err := s.Send(t.Context(), "person@example.com", "Confirm your address", "the body"); err != nil {
		t.Fatalf("sending: %v", err)
	}
	if got == nil {
		t.Fatal("no request was made")
	}
	if got.Method != http.MethodPost {
		t.Errorf("method is %s, want POST", got.Method)
	}
	if got.URL.String() != "https://api.resend.com/emails" {
		t.Errorf("endpoint is %q, want Resend's send endpoint", got.URL)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer re_secret_key" {
		t.Errorf("Authorization is %q, want the bearer key", h)
	}
	if h := got.Header.Get("Content-Type"); h != "application/json" {
		t.Errorf("Content-Type is %q, want application/json", h)
	}

	var payload struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		Text    string   `json:"text"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("reading the payload %q: %v", body, err)
	}
	if payload.From != "deployer@example.org" {
		t.Errorf("from is %q, want the configured address", payload.From)
	}
	if len(payload.To) != 1 || payload.To[0] != "person@example.com" {
		t.Errorf("to is %v, want the one recipient", payload.To)
	}
	if payload.Subject != "Confirm your address" || payload.Text != "the body" {
		t.Errorf("subject %q and text %q are not what was sent", payload.Subject, payload.Text)
	}
}

// TestSendReportsAProviderRefusalWithoutItsBody is the AC-27 half this package
// owns: a Resend error can echo the request back, and the request body carries
// the raw link token, so the error is the status and nothing else. The
// recipient address and the API key are asserted alongside it as hygiene, not
// because AC-27 names them.
//
// covers: AC-27
func TestSendReportsAProviderRefusalWithoutItsBody(t *testing.T) {
	const recipient = "person@example.com"
	s := mail.New(mail.Options{
		APIKey: "re_secret_key",
		From:   "deployer@example.org",
		Client: clientAnswering(func(*http.Request) (*http.Response, error) {
			return reply(http.StatusUnprocessableEntity,
				`{"message":"invalid to field: person@example.com"}`), nil
		}),
	})

	err := s.Send(t.Context(), recipient, "subject", "the body carries a link token")
	if err == nil {
		t.Fatal("a 422 from the provider was reported as a successful send")
	}
	if strings.Contains(err.Error(), recipient) {
		t.Errorf("the error names the recipient: %q", err)
	}
	if strings.Contains(err.Error(), "re_secret_key") {
		t.Errorf("the error carries the API key: %q", err)
	}
	if strings.Contains(err.Error(), "link token") {
		t.Errorf("the error carries the message body: %q", err)
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("the error does not say what the provider answered: %q", err)
	}
}

// TestSendReportsATransportFailure is AC-25 from below: the provider being
// unreachable is an error here, which identity.Service then swallows into the
// log rather than failing the registration that triggered it.
//
// covers: AC-25
func TestSendReportsATransportFailure(t *testing.T) {
	const recipient = "person@example.com"
	boom := errors.New("dial tcp: connection refused")
	s := mail.New(mail.Options{
		APIKey: "re_secret_key",
		From:   "deployer@example.org",
		Client: clientAnswering(func(*http.Request) (*http.Response, error) { return nil, boom }),
	})

	err := s.Send(t.Context(), recipient, "subject", "body")
	if err == nil {
		t.Fatal("an unreachable provider was reported as a successful send")
	}
	if strings.Contains(err.Error(), recipient) {
		t.Errorf("the error names the recipient: %q", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the transport failure was not wrapped: %q", err)
	}
}

// TestSendCarriesTheCallersContext proves the request is built with the caller's
// context rather than a background one, which is what lets a shutdown or a
// request timeout end a send in flight.
func TestSendCarriesTheCallersContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var reached error

	s := mail.New(mail.Options{
		APIKey: "re_secret_key",
		From:   "deployer@example.org",
		Client: clientAnswering(func(r *http.Request) (*http.Response, error) {
			reached = r.Context().Err()
			return reply(http.StatusOK, "{}"), nil
		}),
	})

	cancel()
	_ = s.Send(ctx, "person@example.com", "subject", "body")
	if !errors.Is(reached, context.Canceled) {
		t.Errorf("the request context reported %v, want the caller's cancellation", reached)
	}
}
