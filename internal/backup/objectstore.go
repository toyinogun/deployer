package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ObjectStore is the bucket a run puts its object in and reads it back from. The
// interface is declared here, where it is used, so the run itself never imports
// an S3 client: a test hands it a fake and exercises the whole path.
type ObjectStore interface {
	// Put uploads the file at path under key, with the size the caller already
	// knows because encryption streamed to disk.
	Put(ctx context.Context, key, path string, size int64) error
	// Get opens the object for reading. The caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// S3Store is an ObjectStore backed by an S3 compatible bucket, which for this
// platform is Cloudflare R2.
type S3Store struct {
	client *minio.Client
	bucket string
}

// S3Options is what an S3Store needs to reach its bucket.
type S3Options struct {
	// Endpoint is the S3 compatible host, with its scheme.
	Endpoint string
	Bucket   string
	// Region defaults to "auto", which is what R2 wants.
	Region          string
	AccessKeyID     string
	SecretAccessKey string
}

// NewS3Store builds a client for one bucket. The credential is scoped to that
// bucket by the token that issued it, not by anything here.
func NewS3Store(opts S3Options) (*S3Store, error) {
	host, secure, err := splitEndpoint(opts.Endpoint)
	if err != nil {
		return nil, err
	}
	if opts.Region == "" {
		opts.Region = "auto"
	}
	client, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(opts.AccessKeyID, opts.SecretAccessKey, ""),
		Secure: secure,
		Region: opts.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("backup: building the bucket client: %w", err)
	}
	return &S3Store{client: client, bucket: opts.Bucket}, nil
}

// Put uploads the file at path. The size is passed rather than discovered, so a
// truncated or still growing file is a failure here rather than a short object
// nobody notices until restore day.
func (s *S3Store) Put(ctx context.Context, key, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("backup: opening the encrypted file to upload: %w", err)
	}
	defer func() { _ = f.Close() }()

	// The bucket name and the key are deliberately absent from this error. A
	// failure that reaches an alert must not carry where the backups live
	// (AC-14).
	if _, err := s.client.PutObject(ctx, s.bucket, key, f, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	}); err != nil {
		return fmt.Errorf("backup: uploading the object: %w", err)
	}
	return nil
}

// Get opens the object for reading, which is how a run reads its own upload back
// and how restore fetches one.
func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("backup: opening the object: %w", err)
	}
	// GetObject issues no request: it hands back a reader that fetches on the
	// first Read. Without this, a mistyped key or an expired credential surfaces
	// inside age.Decrypt and restore reports a decryption failure, which points
	// the operator at the one thing they cannot re-derive. Stat makes the fetch
	// happen here, where the error is still its own. The message stays free of
	// the bucket and the key, as every error that can reach an alert does
	// (AC-14).
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, fmt.Errorf("backup: reading the object: %w", err)
	}
	return obj, nil
}

// splitEndpoint turns a configured endpoint into the host and the scheme flag
// minio-go takes separately. It is validated at startup in internal/config as
// well; this is the parse that has to produce the two pieces.
func splitEndpoint(endpoint string) (host string, secure bool, err error) {
	switch {
	case len(endpoint) > 8 && endpoint[:8] == "https://":
		return endpoint[8:], true, nil
	case len(endpoint) > 7 && endpoint[:7] == "http://":
		return endpoint[7:], false, nil
	default:
		return "", false, fmt.Errorf("backup: the bucket endpoint must start with http:// or https://")
	}
}

// verifyObject reads the object back and compares its byte length and SHA-256
// against what was written. A mismatch fails the run and the object is left in
// place: a partly readable backup is worth more than none, and the bucket's
// retention rule would refuse the delete anyway (AC-6).
func verifyObject(ctx context.Context, store ObjectStore, key string, want encrypted) error {
	r, err := store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("backup: reading the object back: %w", err)
	}
	defer func() { _ = r.Close() }()

	sum := sha256.New()
	n, err := io.Copy(sum, r)
	if err != nil {
		return fmt.Errorf("backup: reading the object back: %w", err)
	}
	if n != want.Size {
		return fmt.Errorf("backup: the object read back is %d bytes, %d were written", n, want.Size)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want.SHA256 {
		return fmt.Errorf("backup: the object read back does not match the checksum of what was written")
	}
	return nil
}
