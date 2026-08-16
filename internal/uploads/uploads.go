// Package uploads owns the source tarball: accepting one, recording what it is,
// and spending the single use token a build presents to fetch it back. It never
// imports the store, net/http, or client-go; it declares the narrow interface it
// needs and takes a reader.
package uploads

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/toyinogun/deployer/internal/ids"
)

// Window is how long an upload stays usable. One hour is spec 0001's number: an
// agent uploads and deploys in one breath, so anything longer is only a wider
// window for a leaked id.
const Window = time.Hour

// gzipMagic is the first two bytes of every gzip stream. Checking it is not
// validation of the archive, only a cheap refusal of a body that was plainly
// never gzip, before the platform spends a volume on it.
var gzipMagic = [2]byte{0x1f, 0x8b}

// The failures callers branch on.
var (
	// ErrTooLarge means the body exceeded the configured cap. The write stops at
	// the cap rather than after it, so a hostile body never lands whole.
	ErrTooLarge = errors.New("uploads: body exceeds the maximum upload size")

	// ErrNotGzip means the body did not start as a gzip stream.
	ErrNotGzip = errors.New("uploads: body is not gzip")

	// ErrNotFound means no upload exists under that id or token.
	ErrNotFound = errors.New("uploads: not found")

	// ErrExpired means the upload's window has passed.
	ErrExpired = errors.New("uploads: expired")

	// ErrRedeemed means the single use fetch token was already spent.
	ErrRedeemed = errors.New("uploads: already redeemed")

	// ErrTooManyUnclaimed means the account already holds as many uploads that
	// no deploy has claimed as it may. It bounds what one valid token can cost
	// the volume, which is a bound the tailnet used to provide by keeping the
	// endpoint unreachable (spec 0022, AC-17).
	ErrTooManyUnclaimed = errors.New("uploads: too many unclaimed uploads")
)

// Upload is one recorded tarball, carrying only what this package and its
// callers read.
type Upload struct {
	ID        string
	AccountID string
	Path      string
	SizeBytes int64
	SHA256    string
	ExpiresAt string
	// Redeemed says the single use fetch token has already been spent, which is
	// what makes an upload id unusable for a second deploy.
	Redeemed bool
}

// New describes a tarball that has landed on the volume.
type New struct {
	ID             string
	AccountID      string
	Path           string
	SizeBytes      int64
	SHA256         string
	FetchTokenHash string
	ExpiresAt      string
}

// Sweepable is one expired upload nothing references: the row to delete and the
// file that goes with it.
type Sweepable struct {
	ID   string
	Path string
}

// Store is the slice of persistence this package needs. internal/store
// satisfies it through the adapter in that package.
type Store interface {
	// CreateUpload records a tarball and the token a build will present for it,
	// refusing one that would take the account past limit unclaimed uploads with
	// ErrTooManyUnclaimed. The count and the insert are one transaction, so the
	// cap cannot be raced.
	CreateUpload(ctx context.Context, u New, limit int) (Upload, error)
	// CountUnclaimed is how many unexpired uploads the account holds that no
	// deploy has claimed. It answers the same question CreateUpload asks inside
	// its transaction, and exists so the refusal can happen before a byte
	// reaches the volume rather than after (spec 0022, AC-17).
	CountUnclaimed(ctx context.Context, accountID string) (int, error)
	// ListSweepable reads the expired uploads no deployment references.
	ListSweepable(ctx context.Context) ([]Sweepable, error)
	// DeleteRow removes one upload row.
	DeleteRow(ctx context.Context, id string) error
	// GetUpload reads one upload, or ErrNotFound.
	GetUpload(ctx context.Context, id string) (Upload, error)
	// RedeemUpload spends a fetch token, once, returning ErrRedeemed, ErrExpired,
	// or ErrNotFound when it cannot.
	RedeemUpload(ctx context.Context, fetchTokenHash string) (Upload, error)
	// SetFetchToken replaces an upload's fetch token hash and clears its
	// redemption, so the reconcile loop can mint a fresh token for each build
	// attempt without a restart between upload and build losing anything.
	SetFetchToken(ctx context.Context, uploadID, fetchTokenHash string) error
}

// Service accepts uploads onto a directory and records them.
type Service struct {
	store    Store
	dir      string
	maxBytes int64
	// maxUnclaimed is how many uploads one account may hold that no deploy has
	// claimed. Zero or less means no cap.
	maxUnclaimed int
	clock        ids.Clock
}

// NewService returns a service writing into dir, refusing bodies over maxBytes
// and accounts already holding maxUnclaimed uploads no deploy has claimed.
func NewService(s Store, dir string, maxBytes int64, maxUnclaimed int, clock ids.Clock) *Service {
	if clock == nil {
		clock = ids.SystemClock{}
	}
	return &Service{store: s, dir: dir, maxBytes: maxBytes, maxUnclaimed: maxUnclaimed, clock: clock}
}

// Accept streams body onto the upload volume, hashing and measuring it on the
// way, and records what landed.
//
// Nothing the caller sent decides where the file goes: the path is the upload
// directory joined with an id this platform generated, so no request can steer
// a write. The body stops being read one byte past the cap, so an oversized or
// endless body costs one byte more than the limit rather than a full volume.
func (s *Service) Accept(ctx context.Context, accountID string, body io.Reader) (Upload, error) {
	// The cap, asked before anything is written, so a refused upload costs the
	// volume nothing at all. It is not the guard: CreateUpload asks the same
	// question inside the transaction that inserts the row, which is what makes
	// the cap exact under two racing uploads. This one only makes the ordinary
	// refusal cheap (spec 0022, AC-17).
	if s.maxUnclaimed > 0 {
		held, err := s.store.CountUnclaimed(ctx, accountID)
		if err != nil {
			return Upload{}, err
		}
		if held >= s.maxUnclaimed {
			return Upload{}, ErrTooManyUnclaimed
		}
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Upload{}, fmt.Errorf("uploads: preparing %s: %w", s.dir, err)
	}
	now := s.clock.Now()
	id := ids.New(ids.Upload, now)
	path := filepath.Join(s.dir, id)

	size, sum, err := s.write(path, body)
	if err != nil {
		s.discard(ctx, path)
		return Upload{}, err
	}

	// The column is NOT NULL and UNIQUE, and no build may fetch this yet, so it
	// is seeded with the hash of a random value that is thrown away unread. The
	// usable token is minted later, by the loop that composes the build Job.
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		s.discard(ctx, path)
		return Upload{}, fmt.Errorf("uploads: generating the placeholder fetch token: %w", err)
	}
	placeholder := sha256.Sum256(seed)

	up, err := s.store.CreateUpload(ctx, New{
		ID:             id,
		AccountID:      accountID,
		Path:           path,
		SizeBytes:      size,
		SHA256:         sum,
		FetchTokenHash: hex.EncodeToString(placeholder[:]),
		ExpiresAt:      ids.Stamp(now.Add(Window)),
	}, s.maxUnclaimed)
	if err != nil {
		s.discard(ctx, path)
		return Upload{}, err
	}
	return up, nil
}

// write streams r to path, capped, returning the bytes written and their hash.
func (s *Service) write(path string, r io.Reader) (int64, string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", fmt.Errorf("uploads: creating %s: %w", path, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Error("closing an upload file failed", "error", err, "path", path)
		}
	}()

	// One byte past the cap, so reaching the limit is distinguishable from a body
	// that happened to be exactly the maximum size.
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(r, s.maxBytes+1))
	if err != nil {
		return 0, "", fmt.Errorf("uploads: writing %s: %w", path, err)
	}
	if size > s.maxBytes {
		return 0, "", ErrTooLarge
	}
	if size < int64(len(gzipMagic)) {
		return 0, "", ErrNotGzip
	}
	if err := checkGzip(path); err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(hasher.Sum(nil)), nil
}

// checkGzip reads back the first two bytes and refuses a body that was plainly
// never a gzip stream.
func checkGzip(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("uploads: reading back %s: %w", path, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Error("closing an upload file failed", "error", err, "path", path)
		}
	}()
	var head [2]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return ErrNotGzip
	}
	if head != gzipMagic {
		return ErrNotGzip
	}
	return nil
}

// Open reads a stored tarball back off the volume. The reconcile loop walks its
// tar headers to choose a build engine, and the fetch endpoint serves it, so this
// is the only way anything outside this package reaches the bytes.
//
// It opens read only and never rewrites: an upload is immutable once its hash was
// recorded, and the hash is what the build pod checks the download against.
func (s *Service) Open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("uploads: opening %s: %w", path, err)
	}
	return f, nil
}

// discard removes a file an upload never finished claiming. A failure here
// leaves a stray file the retention sweep will not know about, so it is logged
// loudly rather than dropped.
func (s *Service) discard(ctx context.Context, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.ErrorContext(ctx, "removing an abandoned upload failed", "error", err, "path", path)
	}
}

// Remove deletes an upload's file from the volume. The reconcile loop calls it
// once a deployment reaches any terminal state, so a tarball never outlives the
// build it was for (spec 0004, AC-22).
func (s *Service) Remove(ctx context.Context, path string) {
	if path == "" {
		return
	}
	s.discard(ctx, path)
}

// Redeem spends a fetch token and returns the upload it unlocks. Single use is
// the store's conditional update rather than a read followed by a write, so two
// builds presenting the same token cannot both win.
func (s *Service) Redeem(ctx context.Context, rawToken string) (Upload, error) {
	if rawToken == "" {
		return Upload{}, ErrNotFound
	}
	sum := sha256.Sum256([]byte(rawToken))
	return s.store.RedeemUpload(ctx, hex.EncodeToString(sum[:]))
}

// MintFetchToken generates the single use token a build will present, records
// its hash against the upload, and returns the raw value.
//
// The raw value goes straight onto the build Job's init container and is never
// persisted or logged. Minting again resets the redemption, so a resumed or
// retried build gets a working token rather than a spent one.
func (s *Service) MintFetchToken(ctx context.Context, uploadID string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("uploads: generating a fetch token: %w", err)
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	if err := s.store.SetFetchToken(ctx, uploadID, hex.EncodeToString(sum[:])); err != nil {
		return "", err
	}
	return token, nil
}

// Get reads one upload.
func (s *Service) Get(ctx context.Context, id string) (Upload, error) {
	return s.store.GetUpload(ctx, id)
}

// Sweep removes every expired upload no deployment references, the file and the
// row together, and reports how many it took.
//
// An upload a deployment still names is left alone, so deploy history stays
// intact: the row carries the source a release was built from and deleting it is
// what ON DELETE RESTRICT is there to refuse. The query excludes those rather
// than the sweep discovering them by error (spec 0022, AC-18).
//
// The file goes before the row. A row whose file is already gone is a case the
// fetch path handles and answers as expired, while a file whose row is gone is a
// leak nothing would ever find again, so the order that fails safely is this one.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	rows, err := s.store.ListSweepable(ctx)
	if err != nil {
		return 0, err
	}
	swept := 0
	for _, row := range rows {
		s.discard(ctx, row.Path)
		if err := s.store.DeleteRow(ctx, row.ID); err != nil {
			// One row that will not delete must not stop the rest. The next
			// sweep sees it again, and its file is already gone.
			slog.ErrorContext(ctx, "deleting a swept upload row failed", "error", err, "upload", row.ID)
			continue
		}
		swept++
	}
	return swept, nil
}
