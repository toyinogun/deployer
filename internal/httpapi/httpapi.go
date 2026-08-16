// Package httpapi is the platform's plain HTTP surface: the upload endpoint an
// agent posts a tarball to, and the single use fetch endpoint a build's init
// container reads it back from. The MCP tool surface lives elsewhere.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/uploads"
)

// Options is what the machine surface needs from configuration.
type Options struct {
	// MaxBytes is the ceiling a request body is refused over. It is strictly
	// under the edge's own body cap, so the platform is always the thing that
	// refuses (spec 0022, AC-11).
	MaxBytes int64
	// MCPHost is the public deploy hostname. The upload endpoint and the MCP
	// endpoint are registered a second time under it and everything else on that
	// host answers 404 (spec 0022, AC-2).
	MCPHost string
	// TrustedHosts are the platform's public hostnames, the ones
	// CF-Connecting-IP is read on. The same set every other surface holds
	// (spec 0022, AC-13, AC-14).
	TrustedHosts []string
}

// API holds everything the HTTP handlers need.
type API struct {
	auth    *auth.Authenticator
	auditor auth.Auditor
	uploads *uploads.Service
	limiter *identity.Limiter
	opts    Options
}

// New returns the HTTP surface. limiter is the deploy path's own bucket, kept
// apart from the sign in one so a burst here can never lock a person out of the
// console (spec 0022, AC-15).
func New(a *auth.Authenticator, auditor auth.Auditor, u *uploads.Service,
	limiter *identity.Limiter, opts Options,
) *API {
	return &API{auth: a, auditor: auditor, uploads: u, limiter: limiter, opts: opts}
}

// Register adds this package's routes to mux, and the MCP endpoint beside them
// because the two share a hostname and a bearer token.
//
// Registration under the deploy host is opt in, exactly as it is for the console
// in internal/web: each route that answers there is registered under the deploy
// host pattern, and one catch all answers 404 for the rest. A route added to
// this mux later is absent from the deploy host until somebody registers it
// there, so the direction this fails in is the private one (spec 0022, AC-2).
//
// The deploy routes are registered on that pattern and on no other. They used to
// be registered on the default pattern too, which is what the tailnet name
// reaches, and spec 0022's last step retired that half once a real deploy had
// run through the public one (AC-5, AC-21). So an empty MCPHost now leaves the
// deploy path served nowhere rather than served on the tailnet. That is the safe
// direction, and it cannot happen on a real boot: internal/config requires
// DEPLOYER_MCP_HOST, so only a test constructs an API without one.
//
// GET /v1/uploads/{id} is deliberately not among them, and it is the one route
// that must stay on the default pattern. It is the single use fetch a build's
// init container reads over cluster DNS, which knows no public name, so it stays
// off the deploy host and off the internet (AC-4).
func (a *API) Register(mux *http.ServeMux, mcpHandler http.Handler) {
	// public is every route the deploy host serves, and the deploy host is the
	// only place they answer.
	public := []struct {
		pattern string
		handler http.Handler
	}{
		{"POST /v1/uploads", http.HandlerFunc(a.createUpload)},
		{"/mcp", mcpHandler},
	}
	for _, route := range public {
		if route.handler == nil || a.opts.MCPHost == "" {
			continue
		}
		mux.Handle(withHost(route.pattern, a.opts.MCPHost), route.handler)
	}
	mux.HandleFunc("GET /v1/uploads/{id}", a.fetchUpload)

	// The catch all, last, in the same shape as the console's. It carries no
	// method, so it takes every verb, and it is a subtree pattern, so every path
	// the loop above did not claim on this host lands here. A refusal writes no
	// audit row: no caller has been identified at this point (AC-2).
	if a.opts.MCPHost != "" {
		mux.HandleFunc(a.opts.MCPHost+"/", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}
}

// withHost puts host in front of a mux pattern's path, keeping the method where
// the pattern has one. `POST /v1/uploads` under `mcp.example.org` becomes
// `POST mcp.example.org/v1/uploads`, which is the form the standard mux has taken
// since Go 1.22. The page surface has its own copy for the console host; the two
// are six lines of string handling with no rule in them.
func withHost(pattern, host string) string {
	method, path, found := strings.Cut(pattern, " ")
	if !found {
		return host + pattern
	}
	return method + " " + host + path
}

// createUpload accepts a gzipped tar body and records it.
func (a *API) createUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Derived once, so every use below is the same visitor by construction rather
	// than by six call sites agreeing.
	addr := a.address(r)
	// The token bucket is spent here rather than inside the authenticator,
	// because it bounds the call rate rather than judging the credentials. The
	// lockout on repeated bad tokens is the other way round and lives in
	// Authenticate, so both routes inherit it (spec 0022, AC-15, AC-16).
	if !a.limiter.Allow(addr) {
		a.denyUpload(ctx, w, addr, "", domain.ReasonTooManyAttempts, http.StatusTooManyRequests)
		return
	}
	account, err := a.auth.Authenticate(ctx, auth.BearerToken(r.Header.Get("Authorization")), addr)
	// A suspended account presented a working credential, so it is refused as a
	// decision rather than as a bad credential: 403 with the closed reason code,
	// and audited against the account it actually belongs to (spec 0018, AC-11).
	if errors.Is(err, auth.ErrAccountSuspended) {
		a.denyUpload(ctx, w, addr, account.ID, domain.ReasonAccountSuspended, http.StatusForbidden)
		return
	}
	// The penalty a run of bad tokens earned. The rule itself is in the
	// authenticator, so this only names the outcome (spec 0022, AC-16).
	if errors.Is(err, auth.ErrTooManyAttempts) {
		a.denyUpload(ctx, w, addr, "", domain.ReasonTooManyAttempts, http.StatusTooManyRequests)
		return
	}
	if err != nil {
		// The denial is audited with a null account, which is exactly the row
		// worth having: something presented a credential and it did not work.
		auth.Record(ctx, a.auditor, auth.Audit{ClientAddress: addr, Action: auth.ActionUpload, Reason: "token invalid"})
		if !errors.Is(err, auth.ErrTokenInvalid) {
			slog.ErrorContext(ctx, "authenticating an upload failed", "error", err)
		}
		writeError(ctx, w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// A declared length over the cap is refused before a single byte is read, so
	// an honest oversized client never spends the volume or the transfer.
	if r.ContentLength > a.opts.MaxBytes {
		a.denyUpload(ctx, w, addr, account.ID, domain.ReasonUploadTooLarge, http.StatusRequestEntityTooLarge)
		return
	}

	// MaxBytesReader is the second gate, for a body that declared nothing or
	// lied. The service caps too; this stops the read at the socket.
	up, err := a.uploads.Accept(ctx, account.ID, http.MaxBytesReader(w, r.Body, a.opts.MaxBytes+1))
	switch {
	case errors.Is(err, uploads.ErrTooLarge):
		a.denyUpload(ctx, w, addr, account.ID, domain.ReasonUploadTooLarge, http.StatusRequestEntityTooLarge)
		return
	case errors.Is(err, uploads.ErrNotGzip):
		a.denyUpload(ctx, w, addr, account.ID, domain.ReasonUploadNotGzip, http.StatusBadRequest)
		return
	case errors.Is(err, uploads.ErrTooManyUnclaimed):
		// Nothing reached the volume: the count and the insert are one
		// transaction in the store, and the file is removed when it fails
		// (spec 0022, AC-17).
		a.denyUpload(ctx, w, addr, account.ID, domain.ReasonUploadLimitReached, http.StatusTooManyRequests)
		return
	case err != nil:
		slog.ErrorContext(ctx, "accepting an upload failed", "error", err, "account", account.ID)
		a.denyUpload(ctx, w, addr, account.ID, domain.ReasonInternal, http.StatusInternalServerError)
		return
	}

	auth.Record(ctx, a.auditor, auth.Audit{
		ClientAddress: addr,
		AccountID:     account.ID, Action: auth.ActionUpload,
		TargetType: "upload", TargetID: up.ID, Allowed: true,
	})
	// The id and the expiry, and nothing else. The path on the volume, the hash,
	// and the size are the platform's business (AC-2).
	writeJSON(ctx, w, http.StatusCreated, map[string]string{
		"upload_id":  up.ID,
		"expires_at": up.ExpiresAt,
	})
}

// denyUpload records the refusal and answers it. The reason a caller is told and
// the reason stored on the audit row are the same closed code, never a wrapped
// error string (spec 0022, AC-19).
func (a *API) denyUpload(ctx context.Context, w http.ResponseWriter, addr, accountID string,
	reason domain.Reason, status int,
) {
	auth.Record(ctx, a.auditor, auth.Audit{
		ClientAddress: addr,
		AccountID:     accountID, Action: auth.ActionUpload, Reason: string(reason),
	})
	// The code is what a caller branches on and the sentence is what it reads.
	// Both come from the closed set, so neither can carry anything internal.
	writeJSON(ctx, w, status, map[string]string{
		"error":   string(reason),
		"message": reason.Message(),
	})
}

// fetchUpload hands the tarball to whoever presents its single use token. The
// token is the whole credential here: it is not an account token, it works once,
// and it expires with the upload.
func (a *API) fetchUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	up, err := a.uploads.Redeem(ctx, auth.BearerToken(r.Header.Get("Authorization")))
	switch {
	case errors.Is(err, uploads.ErrNotFound):
		writeError(ctx, w, http.StatusUnauthorized, "unauthorized")
		return
	case errors.Is(err, uploads.ErrRedeemed):
		writeError(ctx, w, http.StatusConflict, "already redeemed")
		return
	case errors.Is(err, uploads.ErrExpired):
		writeError(ctx, w, http.StatusGone, "expired")
		return
	case err != nil:
		slog.ErrorContext(ctx, "redeeming an upload failed", "error", err)
		writeError(ctx, w, http.StatusInternalServerError, "internal error")
		return
	}
	// A token that unlocks a different upload than the one asked for is a bad
	// token, and is told so in the same words as an unknown one. The redemption
	// is already spent, which is the safe direction.
	if up.ID != r.PathValue("id") {
		writeError(ctx, w, http.StatusUnauthorized, "unauthorized")
		return
	}

	auth.Record(ctx, a.auditor, auth.Audit{
		ClientAddress: a.address(r),
		AccountID:     up.AccountID, Action: auth.ActionFetchSource,
		TargetType: "upload", TargetID: up.ID, Allowed: true,
	})

	f, err := os.Open(up.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// The tarball is deleted the moment its deployment reaches a terminal
		// state, so a build still fetching after its own deployment was cancelled
		// or timed out finds nothing. That is the platform working, and it is the
		// same answer as an upload that expired, in the same words.
		slog.InfoContext(ctx, "an upload was fetched after its deployment ended", "upload", up.ID)
		writeError(ctx, w, http.StatusGone, "expired")
		return
	case err != nil:
		slog.ErrorContext(ctx, "opening an upload failed", "error", err, "upload", up.ID)
		writeError(ctx, w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.ErrorContext(ctx, "closing an upload failed", "error", err, "upload", up.ID)
		}
	}()

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.FormatInt(up.SizeBytes, 10))
	http.ServeContent(w, r, up.ID, zeroTime, f)
}

// writeJSON writes a JSON body, logging a failed write rather than swallowing it.
func writeJSON(ctx context.Context, w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.WarnContext(ctx, "writing a JSON response failed", "error", err)
	}
}

// writeError answers with one short message. Nothing internal crosses this
// boundary: the caller gets a status and a sentence, never a wrapped error.
func writeError(ctx context.Context, w http.ResponseWriter, status int, message string) {
	writeJSON(ctx, w, status, map[string]string{"error": message})
}

// zeroTime tells ServeContent there is no modification time worth reporting, so
// it serves the bytes without inviting a conditional request.
var zeroTime = time.Time{}

// address is who a call on this surface is attributed to and whose bucket it
// spends from. It passes the platform's public hostnames rather than an empty
// set, because the upload endpoint now answers on the deploy host, where
// CF-Connecting-IP is real. Every surface passes the same set, so one visitor is
// one address everywhere rather than one per surface (spec 0022, AC-13, AC-14).
func (a *API) address(r *http.Request) string {
	return auth.ClientAddress(r, a.opts.TrustedHosts...)
}
