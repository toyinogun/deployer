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
	AccountID      string
	Path           string
	SizeBytes      int64
	SHA256         string
	FetchTokenHash string
	ExpiresAt      string // RFC3339
}

// CreateUpload records an uploaded tarball and the single use token a build will
// present to fetch it.
func (s *Store) CreateUpload(ctx context.Context, u NewUpload) (Upload, error) {
	now := s.now()
	up, err := s.q.CreateUpload(ctx, sqlcgen.CreateUploadParams{
		ID:             ids.New(ids.Upload, s.clock.Now()),
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
		return Upload{}, fmt.Errorf("store: recording upload: %w", err)
	}
	return up, nil
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
