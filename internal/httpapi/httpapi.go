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
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/uploads"
)

// API holds everything the HTTP handlers need.
type API struct {
	auth     *auth.Authenticator
	auditor  auth.Auditor
	uploads  *uploads.Service
	maxBytes int64
}

// New returns the HTTP surface.
func New(a *auth.Authenticator, auditor auth.Auditor, u *uploads.Service, maxBytes int64) *API {
	return &API{auth: a, auditor: auditor, uploads: u, maxBytes: maxBytes}
}

// Register adds this package's routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/uploads", a.createUpload)
	mux.HandleFunc("GET /v1/uploads/{id}", a.fetchUpload)
}

// createUpload accepts a gzipped tar body and records it.
func (a *API) createUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	account, err := a.auth.Authenticate(ctx, auth.BearerToken(r.Header.Get("Authorization")))
	if err != nil {
		// The denial is audited with a null account, which is exactly the row
		// worth having: something presented a credential and it did not work.
		auth.Record(ctx, a.auditor, auth.Audit{Action: auth.ActionUpload, Reason: "token invalid"})
		if !errors.Is(err, auth.ErrTokenInvalid) {
			slog.ErrorContext(ctx, "authenticating an upload failed", "error", err)
		}
		writeError(ctx, w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// A declared length over the cap is refused before a single byte is read, so
	// an honest oversized client never spends the volume or the transfer.
	if r.ContentLength > a.maxBytes {
		a.denyUpload(ctx, w, account.ID, "too large", http.StatusRequestEntityTooLarge,
			"body exceeds the maximum upload size")
		return
	}

	// MaxBytesReader is the second gate, for a body that declared nothing or
	// lied. The service caps too; this stops the read at the socket.
	up, err := a.uploads.Accept(ctx, account.ID, http.MaxBytesReader(w, r.Body, a.maxBytes+1))
	switch {
	case errors.Is(err, uploads.ErrTooLarge):
		a.denyUpload(ctx, w, account.ID, "too large", http.StatusRequestEntityTooLarge,
			"body exceeds the maximum upload size")
		return
	case errors.Is(err, uploads.ErrNotGzip):
		a.denyUpload(ctx, w, account.ID, "not gzip", http.StatusBadRequest,
			"body must be a gzipped tar archive")
		return
	case err != nil:
		slog.ErrorContext(ctx, "accepting an upload failed", "error", err, "account", account.ID)
		a.denyUpload(ctx, w, account.ID, "internal", http.StatusInternalServerError, "internal error")
		return
	}

	auth.Record(ctx, a.auditor, auth.Audit{
		AccountID: account.ID, Action: auth.ActionUpload,
		TargetType: "upload", TargetID: up.ID, Allowed: true,
	})
	// The id and the expiry, and nothing else. The path on the volume, the hash,
	// and the size are the platform's business (AC-2).
	writeJSON(ctx, w, http.StatusCreated, map[string]string{
		"upload_id":  up.ID,
		"expires_at": up.ExpiresAt,
	})
}

// denyUpload records the refusal and answers it.
func (a *API) denyUpload(ctx context.Context, w http.ResponseWriter, accountID, reason string, status int, message string) {
	auth.Record(ctx, a.auditor, auth.Audit{
		AccountID: accountID, Action: auth.ActionUpload, Reason: reason,
	})
	writeError(ctx, w, status, message)
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
		AccountID: up.AccountID, Action: auth.ActionFetchSource,
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
