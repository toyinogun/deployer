package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// uploadOrphanWindow is how long a spent or expired upload row lingers before
// the sweep takes it, matching the orphan window in spec 0001.
const uploadOrphanWindow = 24 * time.Hour

// NewUpload describes a tarball that has landed on the upload volume.
type NewUpload struct {
	// ID is the row's primary key. The caller supplies it when the file on disk
	// is already named after it, which is the normal path: the upload service
	// generates the id first so nothing the caller sent decides where the file
	// goes. Empty means generate one here.
	ID             string
	AccountID      string
	Path           string
	SizeBytes      int64
	SHA256         string
	FetchTokenHash string
	ExpiresAt      string // RFC3339
}

// CreateUpload records an uploaded tarball and the single use token a build will
// present to fetch it, refusing one that would take the account past limit
// unclaimed uploads.
//
// The count and the insert are one transaction, in the same shape CreateApp uses
// for the per account app cap: inTx opens with BEGIN IMMEDIATE, so the write lock
// is already held when the count runs and two racing uploads cannot both pass the
// cap. That also means any future caller reaches the cap by using this method
// rather than by repeating the check (spec 0022, AC-17).
//
// A limit of zero or less means no cap, which is what a caller that has no
// business enforcing one passes.
func (s *Store) CreateUpload(ctx context.Context, u NewUpload, limit int) (Upload, error) {
	now := s.now()
	if u.ID == "" {
		u.ID = ids.New(ids.Upload, s.clock.Now())
	}
	var created Upload
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		if limit > 0 {
			held, err := q.CountUnclaimedUploads(ctx, sqlcgen.CountUnclaimedUploadsParams{
				AccountID: u.AccountID,
				Now:       now,
			})
			if err != nil {
				return fmt.Errorf("store: counting unclaimed uploads for account %s: %w", u.AccountID, err)
			}
			if held >= int64(limit) {
				return ErrUploadLimit
			}
		}
		up, err := q.CreateUpload(ctx, sqlcgen.CreateUploadParams{
			ID:             u.ID,
			AccountID:      u.AccountID,
			Path:           u.Path,
			SizeBytes:      u.SizeBytes,
			Sha256:         u.SHA256,
			FetchTokenHash: u.FetchTokenHash,
			ExpiresAt:      u.ExpiresAt,
			CreatedAt:      now,
			UpdatedAt:      now,
		})
		if err != nil {
			return fmt.Errorf("store: recording upload: %w", err)
		}
		created = up
		return nil
	})
	if err != nil {
		return Upload{}, err
	}
	return created, nil
}

// CountUnclaimedUploads is how many unexpired uploads the account holds that no
// deploy has claimed. It answers the same question CreateUpload asks inside its
// transaction, for a caller that wants to refuse before it spends the volume.
func (s *Store) CountUnclaimedUploads(ctx context.Context, accountID string) (int, error) {
	n, err := s.q.CountUnclaimedUploads(ctx, sqlcgen.CountUnclaimedUploadsParams{
		AccountID: accountID,
		Now:       s.now(),
	})
	if err != nil {
		return 0, fmt.Errorf("store: counting unclaimed uploads for account %s: %w", accountID, err)
	}
	return int(n), nil
}

// SweepableUpload is one expired upload no deployment references: the row to
// delete and the file that goes with it.
type SweepableUpload struct {
	ID   string
	Path string
}

// ListSweepableUploads reads the expired uploads nothing still references. The
// caller removes each file and then calls DeleteUpload, so the row outlives the
// file rather than the other way round: a row with no file is already a handled
// case on the fetch path, and a file with no row is a leak nothing would ever
// find (spec 0022, AC-18).
func (s *Store) ListSweepableUploads(ctx context.Context) ([]SweepableUpload, error) {
	rows, err := s.q.ListSweepableUploads(ctx, s.now())
	if err != nil {
		return nil, fmt.Errorf("store: listing sweepable uploads: %w", err)
	}
	out := make([]SweepableUpload, 0, len(rows))
	for _, r := range rows {
		out = append(out, SweepableUpload{ID: r.ID, Path: r.Path})
	}
	return out, nil
}

// DeleteUpload removes one upload row. A row a deployment still names is
// protected by ON DELETE RESTRICT and is never handed here, because
// ListSweepableUploads excludes it.
func (s *Store) DeleteUpload(ctx context.Context, id string) error {
	if _, err := s.q.DeleteUpload(ctx, id); err != nil {
		return fmt.Errorf("store: deleting upload %s: %w", id, err)
	}
	return nil
}

// GetUpload reads one upload.
func (s *Store) GetUpload(ctx context.Context, id string) (Upload, error) {
	up, err := s.q.GetUpload(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Upload{}, ErrNotFound
	}
	if err != nil {
		return Upload{}, fmt.Errorf("store: reading upload %s: %w", id, err)
	}
	return up, nil
}

// RedeemUpload spends a fetch token, once. The single use rule is the update
// itself rather than a read followed by a write, so two builds presenting the
// same token cannot both win.
func (s *Store) RedeemUpload(ctx context.Context, fetchTokenHash string) (Upload, error) {
	now := s.now()
	up, err := s.q.RedeemUpload(ctx, sqlcgen.RedeemUploadParams{
		Now:            ptr(now),
		FetchTokenHash: fetchTokenHash,
	})
	if err == nil {
		return up, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Upload{}, fmt.Errorf("store: redeeming upload: %w", err)
	}
	// Zero rows means already spent, expired, or never existed. Look once more
	// to tell the caller which, since these need different handling upstream.
	var redeemedAt *string
	var expiresAt string
	row := s.db.QueryRowContext(ctx,
		`SELECT redeemed_at, expires_at FROM uploads WHERE fetch_token_hash = ?`, fetchTokenHash)
	if err := row.Scan(&redeemedAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Upload{}, ErrNotFound
		}
		return Upload{}, fmt.Errorf("store: reading the unredeemable upload: %w", err)
	}
	if redeemedAt != nil {
		return Upload{}, ErrUploadRedeemed
	}
	return Upload{}, ErrUploadExpired
}

// SetUploadFetchToken replaces the token a build presents for an upload and
// clears any previous redemption, so a retried build gets a usable token.
func (s *Store) SetUploadFetchToken(ctx context.Context, uploadID, fetchTokenHash string) error {
	n, err := s.q.SetUploadFetchToken(ctx, sqlcgen.SetUploadFetchTokenParams{
		FetchTokenHash: fetchTokenHash,
		Now:            s.now(),
		ID:             uploadID,
	})
	if err != nil {
		return fmt.Errorf("store: setting the fetch token on %s: %w", uploadID, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SweepCounts reports what a retention sweep removed.
type SweepCounts struct {
	Deployments int64
	Events      int64
	Uploads     int64
}

// Sweep applies the retention policy. Children go before parents throughout,
// because nothing cascades: first the events of the terminal deployments that
// are about to go (whatever their own age), then those deployments, then the
// aged events of deployments that survive, then spent or expired uploads that no
// surviving deployment still names. Releases and the audit log are never touched.
func (s *Store) Sweep(ctx context.Context, retention time.Duration) (SweepCounts, error) {
	now := s.clock.Now()
	cutoff := ids.Stamp(now.Add(-retention))
	uploadCutoff := ids.Stamp(now.Add(-uploadOrphanWindow))

	var counts SweepCounts
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		childEvents, err := q.DeleteEventsOfSweptDeployments(ctx, ptr(cutoff))
		if err != nil {
			return fmt.Errorf("store: sweeping the events of expired deployments: %w", err)
		}
		deployments, err := q.DeleteSweptDeployments(ctx, ptr(cutoff))
		if err != nil {
			return fmt.Errorf("store: sweeping expired deployments: %w", err)
		}
		agedEvents, err := q.DeleteAgedEvents(ctx, cutoff)
		if err != nil {
			return fmt.Errorf("store: sweeping aged events: %w", err)
		}
		uploads, err := q.DeleteStaleUploads(ctx, sqlcgen.DeleteStaleUploadsParams{
			UploadCutoff: uploadCutoff,
			Now:          ids.Stamp(now),
		})
		if err != nil {
			return fmt.Errorf("store: sweeping stale uploads: %w", err)
		}
		counts = SweepCounts{
			Deployments: deployments,
			Events:      childEvents + agedEvents,
			Uploads:     uploads,
		}
		return nil
	})
	if err != nil {
		return SweepCounts{}, err
	}
	return counts, nil
}
