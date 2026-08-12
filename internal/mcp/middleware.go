package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/toyinogun/deployer/internal/auth"
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

// accountFrom reads the account the middleware resolved. It cannot be missing on
// a request that reached a tool, because the middleware answers 401 first.
func accountFrom(ctx context.Context) auth.Account {
	account, _ := ctx.Value(accountKey{}).(auth.Account)
	return account
}
