package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
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
		account, err := s.auth.Authenticate(ctx, auth.BearerToken(r.Header.Get("Authorization")))
		// A suspended account presented a working credential, so it is not a
		// transport failure. It is carried through to the protocol layer, where
		// refuseSuspended answers every tool call with account_suspended as a tool
		// result the agent can read (spec 0018, AC-9).
		if errors.Is(err, auth.ErrAccountSuspended) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, accountKey{}, account)))
			return
		}
		if err != nil {
			// Audited with a null account, which is the row worth having:
			// something presented a credential and it did not work (AC-19).
			auth.Record(ctx, s.auditor, auth.Audit{Action: auth.ActionDeploy, Reason: "token invalid"})
			if !errors.Is(err, auth.ErrTokenInvalid) {
				slog.ErrorContext(ctx, "authenticating an MCP call failed", "error", err)
			}
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
