package store

import (
	"context"
	"errors"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/uploads"
)

// UploadStore adapts the store to the narrow interface internal/uploads
// declares, mapping this package's sentinels onto that package's own.
type UploadStore struct{ s *Store }

// ForUploads returns the upload facing view of the store.
func ForUploads(s *Store) UploadStore { return UploadStore{s: s} }

// Compile time proof that the adapter is what internal/uploads asked for.
var _ uploads.Store = UploadStore{}

// row maps a stored upload onto the fields the uploads package reads.
func row(u Upload) uploads.Upload {
	return uploads.Upload{
		ID:        u.ID,
		AccountID: u.AccountID,
		Path:      u.Path,
		SizeBytes: u.SizeBytes,
		SHA256:    u.Sha256,
		ExpiresAt: u.ExpiresAt,
		Redeemed:  u.RedeemedAt != nil,
	}
}

// CreateUpload records a tarball and the token a build will present for it,
// refusing one past the account's unclaimed ceiling.
func (a UploadStore) CreateUpload(ctx context.Context, u uploads.New, limit int) (uploads.Upload, error) {
	up, err := a.s.CreateUpload(ctx, NewUpload{
		ID:             u.ID,
		AccountID:      u.AccountID,
		Path:           u.Path,
		SizeBytes:      u.SizeBytes,
		SHA256:         u.SHA256,
		FetchTokenHash: u.FetchTokenHash,
		ExpiresAt:      u.ExpiresAt,
	}, limit)
	if errors.Is(err, ErrUploadLimit) {
		return uploads.Upload{}, uploads.ErrTooManyUnclaimed
	}
	if err != nil {
		return uploads.Upload{}, err
	}
	return row(up), nil
}

// CountUnclaimed is how many unexpired uploads the account holds that no deploy
// has claimed.
func (a UploadStore) CountUnclaimed(ctx context.Context, accountID string) (int, error) {
	return a.s.CountUnclaimedUploads(ctx, accountID)
}

// ListSweepable reads the expired uploads no deployment references.
func (a UploadStore) ListSweepable(ctx context.Context) ([]uploads.Sweepable, error) {
	rows, err := a.s.ListSweepableUploads(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]uploads.Sweepable, 0, len(rows))
	for _, r := range rows {
		out = append(out, uploads.Sweepable{ID: r.ID, Path: r.Path})
	}
	return out, nil
}

// DeleteRow removes one upload row.
func (a UploadStore) DeleteRow(ctx context.Context, id string) error {
	return a.s.DeleteUpload(ctx, id)
}

// GetUpload reads one upload.
func (a UploadStore) GetUpload(ctx context.Context, id string) (uploads.Upload, error) {
	up, err := a.s.GetUpload(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return uploads.Upload{}, uploads.ErrNotFound
	}
	if err != nil {
		return uploads.Upload{}, err
	}
	return row(up), nil
}

// RedeemUpload spends a fetch token, once.
func (a UploadStore) RedeemUpload(ctx context.Context, fetchTokenHash string) (uploads.Upload, error) {
	up, err := a.s.RedeemUpload(ctx, fetchTokenHash)
	switch {
	case errors.Is(err, ErrNotFound):
		return uploads.Upload{}, uploads.ErrNotFound
	case errors.Is(err, ErrUploadExpired):
		return uploads.Upload{}, uploads.ErrExpired
	case errors.Is(err, ErrUploadRedeemed):
		return uploads.Upload{}, uploads.ErrRedeemed
	case err != nil:
		return uploads.Upload{}, err
	}
	return row(up), nil
}

// SetFetchToken replaces an upload's fetch token hash and clears its redemption.
func (a UploadStore) SetFetchToken(ctx context.Context, uploadID, fetchTokenHash string) error {
	err := a.s.SetUploadFetchToken(ctx, uploadID, fetchTokenHash)
	if errors.Is(err, ErrNotFound) {
		return uploads.ErrNotFound
	}
	return err
}

// RecordAudit writes one authorization outcome.
func (a AuthStore) RecordAudit(ctx context.Context, e auth.Audit) error {
	return a.s.RecordAudit(ctx, AuditEntry{
		AccountID:     e.AccountID,
		Action:        e.Action,
		TargetType:    e.TargetType,
		TargetID:      e.TargetID,
		Allowed:       e.Allowed,
		Reason:        e.Reason,
		ClientAddress: e.ClientAddress,
	})
}

// Compile time proof that the auth adapter also carries the auditing and token
// touching the edges need.
var (
	_ auth.Auditor      = AuthStore{}
	_ auth.TokenToucher = AuthStore{}
)
