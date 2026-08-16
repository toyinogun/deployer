package main

import (
	"context"
	"io"

	"github.com/toyinogun/deployer/internal/mcp"
	"github.com/toyinogun/deployer/internal/reconcile"
	"github.com/toyinogun/deployer/internal/uploads"
)

// uploadSource adapts the upload service to the loop's narrow interface, the
// same way internal/store adapts itself to its callers. It lives here
// because this is the composition root: neither the loop nor the tool surface
// should know the service's own types.
type uploadSource struct{ svc *uploads.Service }

// Compile time proof that each view is what its reader asked for.
var (
	_ reconcile.Uploads = uploadSource{}
	_ mcp.Uploads       = forTool{}
)

// Get reads one upload for the reconcile loop, which needs where it landed and
// what it hashed to.
func (u uploadSource) Get(ctx context.Context, id string) (reconcile.Upload, error) {
	up, err := u.svc.Get(ctx, id)
	if err != nil {
		return reconcile.Upload{}, err
	}
	return reconcile.Upload{ID: up.ID, Path: up.Path, SHA256: up.SHA256}, nil
}

// Open hands the loop the stored tarball, whose tar headers choose the engine.
func (u uploadSource) Open(path string) (io.ReadCloser, error) { return u.svc.Open(path) }

// MintFetchToken generates the single use token one build presents.
func (u uploadSource) MintFetchToken(ctx context.Context, id string) (string, error) {
	return u.svc.MintFetchToken(ctx, id)
}

// Remove deletes a tarball once its deployment is terminal.
func (u uploadSource) Remove(ctx context.Context, path string) { u.svc.Remove(ctx, path) }

// forTool is the tool facing view, which needs ownership and expiry and nothing
// about the volume.
type forTool struct{ svc *uploads.Service }

// Get reads one upload for the tool surface.
func (u forTool) Get(ctx context.Context, id string) (mcp.Upload, error) {
	up, err := u.svc.Get(ctx, id)
	if err != nil {
		return mcp.Upload{}, err
	}
	return forToolUpload(up), nil
}

// Accept records source a deploy carried inline, through the same service the
// upload endpoint uses, so both reach the volume under the same caps.
func (u forTool) Accept(ctx context.Context, accountID string, body io.Reader) (mcp.Upload, error) {
	up, err := u.svc.Accept(ctx, accountID, body)
	if err != nil {
		return mcp.Upload{}, err
	}
	return forToolUpload(up), nil
}

// forToolUpload is the tool facing view of one upload.
func forToolUpload(up uploads.Upload) mcp.Upload {
	return mcp.Upload{
		ID:        up.ID,
		AccountID: up.AccountID,
		ExpiresAt: up.ExpiresAt,
		Redeemed:  up.Redeemed,
	}
}
