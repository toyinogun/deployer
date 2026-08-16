package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/identity"
)

// accountKey is the context key the authenticated account travels under. Its own
// unexported type, so nothing outside this package can put one there.
type accountKey struct{}

// authenticate resolves the bearer token before the MCP transport sees the
// request, so an unauthenticated caller never reaches a tool and never learns
// which tools exist.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		addr := s.address(r)
		// The token bucket, spent here rather than inside the authenticator,
		// because it bounds the call rate rather than judging the credentials.
		// The lockout on repeated bad tokens is the other way round and lives in
		// the authenticator, which is why this handler holds no copy of it
		// (spec 0022, AC-15, AC-16).
		if !s.limiter.Allow(addr) {
			denyTransport(ctx, w, addr, s.auditor, domain.ReasonTooManyAttempts, http.StatusTooManyRequests)
			return
		}
		account, err := s.auth.Authenticate(ctx, auth.BearerToken(r.Header.Get("Authorization")), addr)
		// A suspended account presented a working credential, so it is not a
		// transport failure. It is carried through to the protocol layer, where
		// refuseSuspended answers every tool call with account_suspended as a tool
		// result the agent can read (spec 0018, AC-9).
		if errors.Is(err, auth.ErrAccountSuspended) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, accountKey{}, account)))
			return
		}
		// The penalty a run of bad tokens from this address earned. The rule is
		// in the authenticator, so both routes on the deploy path answer it and
		// neither one decides it (spec 0022, AC-16).
		if errors.Is(err, auth.ErrTooManyAttempts) {
			denyTransport(ctx, w, addr, s.auditor, domain.ReasonTooManyAttempts, http.StatusTooManyRequests)
			return
		}
		if err != nil {
			// Audited with a null account, which is the row worth having:
			// something presented a credential and it did not work (AC-19).
			auth.Record(ctx, s.auditor, auth.Audit{
				ClientAddress: addr, Action: auth.ActionDeploy, Reason: "token invalid"})
			if !errors.Is(err, auth.ErrTokenInvalid) {
				slog.ErrorContext(ctx, "authenticating an MCP call failed", "error", err)
			}
			// Where a client that holds no token should go to get one. It is
			// on this 401 and on no other refusal: the limiter's 429 and a
			// suspended account are not about the credential, and Claude
			// ignores the header on anything but a 401 anyway
			// (spec 0024, AC-2, AC-2a).
			w.Header().Set("WWW-Authenticate", s.challenge())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			if err := json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"}); err != nil {
				slog.WarnContext(ctx, "writing an MCP denial failed", "error", err)
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, accountKey{}, account)))
	})
}

// challenge is the WWW-Authenticate value a 401 on this endpoint carries. It
// points at the protected resource document for this exact path rather than the
// root one, because that document's resource has to match what the person typed
// into their client (spec 0024, AC-1, AC-2).
func (s *Server) challenge() string {
	return fmt.Sprintf("Bearer resource_metadata=%q, scope=%q",
		s.opts.MCPURL+identity.ProtectedResourcePath+"/mcp", identity.ConnectorScope)
}

// denyTransport refuses a call before the MCP transport sees it, with a closed
// reason code and an audit row.
//
// It answers HTTP rather than a tool result on purpose: nothing has been parsed
// yet, so there is no tool call to answer and no session to answer it on. A 429
// with the code is what a client can back off on (spec 0022, AC-19).
func denyTransport(ctx context.Context, w http.ResponseWriter, addr string,
	auditor auth.Auditor, reason domain.Reason, status int,
) {
	auth.Record(ctx, auditor, auth.Audit{
		ClientAddress: addr, Action: auth.ActionDeploy, Reason: string(reason)})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]string{"error": string(reason), "message": reason.Message()}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.WarnContext(ctx, "writing an MCP denial failed", "error", err)
	}
}

// address is who a call on this surface is attributed to and whose bucket it
// spends from. The same derivation the upload endpoint and the pages use, over
// the same set of public hostnames, so one visitor is one address rather than
// one per surface (spec 0022, AC-13, AC-14).
func (s *Server) address(r *http.Request) string {
	return auth.ClientAddress(r, s.opts.TrustedHosts...)
}

// refuseSuspended answers every tool call of a suspended account with
// account_suspended, before the tool handler runs. Registered on the per request
// server only when the calling account is suspended, so a tool added later
// inherits the refusal rather than having to remember it (spec 0018, AC-9).
//
// The refusal is a CallToolResult carrying IsError with a nil Go error, not an
// error return: an error out of a method handler is a protocol error, and an
// agent reads that as a broken connection rather than as a decision it should
// stop retrying and report. This sits above the per tool wrapper that normally
// performs that conversion, so it composes the result shape itself.
func refuseSuspended(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{Text: domain.ReasonAccountSuspended.Message()},
			},
		}, nil
	}
}

// accountFrom reads the account the middleware resolved. It cannot be missing on
// a request that reached a tool, because the middleware answers 401 first.
func accountFrom(ctx context.Context) auth.Account {
	account, _ := ctx.Value(accountKey{}).(auth.Account)
	return account
}
