// This file is in package backup rather than backup_test because splitEndpoint
// and verifyObject are internal: the first is the parse that turns one
// configured string into the two pieces minio-go takes, and the second is the
// read back that decides whether an upload counts.
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// covers: AC-5 - the configured endpoint is split into the host and the scheme
// flag, and anything without a scheme is refused rather than guessed at.
func TestSplitEndpoint(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		endpoint   string
		wantHost   string
		wantSecure bool
		wantErr    bool
	}{
		{name: "https carries the host and turns TLS on", endpoint: "https://abc123.r2.cloudflarestorage.com", wantHost: "abc123.r2.cloudflarestorage.com", wantSecure: true},
		{name: "http carries the host and leaves TLS off", endpoint: "http://minio.test:9000", wantHost: "minio.test:9000"},
		{name: "a path is kept, since the host is passed through whole", endpoint: "https://s3.example.test/prefix", wantHost: "s3.example.test/prefix", wantSecure: true},
		{name: "no scheme is refused", endpoint: "abc123.r2.cloudflarestorage.com", wantErr: true},
		{name: "an empty endpoint is refused", endpoint: "", wantErr: true},
		{name: "a scheme with no host is refused", endpoint: "https://", wantErr: true},
		{name: "a scheme with no host is refused over http too", endpoint: "http://", wantErr: true},
		{name: "another scheme is refused", endpoint: "s3://bucket", wantErr: true},
		{name: "the scheme is matched at the front, not anywhere", endpoint: "example.test/https://", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			host, secure, err := splitEndpoint(tt.endpoint)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("want an error for %q, got host %q", tt.endpoint, host)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitEndpoint(%q): %v", tt.endpoint, err)
			}
			if host != tt.wantHost {
				t.Errorf("host: got %q, want %q", host, tt.wantHost)
			}
			if secure != tt.wantSecure {
				t.Errorf("secure: got %v, want %v", secure, tt.wantSecure)
			}
		})
	}
}

// covers: AC-14 - the refusal names what is wrong with the shape and does not
// echo the endpoint, which on a real deploy carries the account identifier.
func TestSplitEndpoint_theRefusalDoesNotEchoTheEndpoint(t *testing.T) {
	t.Parallel()

	_, _, err := splitEndpoint("abc123secretaccount.r2.cloudflarestorage.com")

	if err == nil {
		t.Fatal("want an error for an endpoint with no scheme")
	}
	if strings.Contains(err.Error(), "abc123secretaccount") {
		t.Errorf("the error echoes the endpoint: %v", err)
	}
}

// A store cannot be built on an endpoint that will not parse, and the failure
// happens here rather than on the first upload.
func TestNewS3Store_refusesAnEndpointWithoutAScheme(t *testing.T) {
	t.Parallel()

	store, err := NewS3Store(S3Options{
		Endpoint: "abc123.r2.cloudflarestorage.com",
		Bucket:   "deployer-backups",
	})

	if err == nil {
		t.Fatalf("want an error, got a store %v", store)
	}
}

// A well formed endpoint builds a client. No request is made here: reaching the
// bucket is what /check verify covers against the real one.
func TestNewS3Store_buildsAClientForAWellFormedEndpoint(t *testing.T) {
	t.Parallel()

	store, err := NewS3Store(S3Options{
		Endpoint:        "https://abc123.r2.cloudflarestorage.com",
		Bucket:          "deployer-backups",
		AccessKeyID:     "key",
		SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	if store == nil || store.bucket != "deployer-backups" {
		t.Fatalf("the store should carry its bucket, got %+v", store)
	}
	// Region is left to the caller only in the sense that empty means auto,
	// which is what R2 wants and what a missing DEPLOYER_BACKUP_S3_REGION gives.
	if _, err := NewS3Store(S3Options{
		Endpoint: "https://abc123.r2.cloudflarestorage.com",
		Bucket:   "deployer-backups",
		Region:   "",
	}); err != nil {
		t.Fatalf("an empty region should default rather than fail: %v", err)
	}
}

// stubStore hands back fixed bytes, or a fixed error, for the read back.
type stubStore struct {
	data []byte
	err  error
}

func (s stubStore) Put(context.Context, string, string, int64) error { return nil }

func (s stubStore) Get(context.Context, string) (io.ReadCloser, error) {
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

// want builds the record of a written object for the given bytes.
func writtenAs(data []byte) encrypted {
	sum := sha256.Sum256(data)
	return encrypted{Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

// covers: AC-6 - an object that reads back at the length and hash it was
// written at is what closes the run.
func TestVerifyObject_passesWhenTheObjectMatches(t *testing.T) {
	t.Parallel()

	data := []byte("age-encryption.org/v1\nthe ciphertext")

	err := verifyObject(t.Context(), stubStore{data: data}, "db/20260815T030000Z-bkp_a.age", writtenAs(data))

	if err != nil {
		t.Fatalf("a matching read back should pass: %v", err)
	}
}

// covers: AC-6 - an upload that succeeds and reads back short is a failed run.
// This is the case the read back exists for: PutObject returned nil and the
// object in the bucket is not the object that was written.
func TestVerifyObject_failsOnAShortReadBack(t *testing.T) {
	t.Parallel()

	data := []byte("age-encryption.org/v1\nthe ciphertext")
	short := data[:len(data)-1]

	err := verifyObject(t.Context(), stubStore{data: short}, "db/object.age", writtenAs(data))

	if err == nil {
		t.Fatal("a short read back should fail the run")
	}
	// The counts are in the message because they are the whole finding, and
	// neither is sensitive.
	if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("the error should name the byte counts, got %v", err)
	}
}

// covers: AC-6 - an object the right length whose bytes differ fails too, which
// length alone would never catch.
func TestVerifyObject_failsOnAChecksumMismatchAtTheSameLength(t *testing.T) {
	t.Parallel()

	written := []byte("age-encryption.org/v1\nthe ciphertext")
	corrupted := append([]byte(nil), written...)
	corrupted[len(corrupted)-1] ^= 0xff

	err := verifyObject(t.Context(), stubStore{data: corrupted}, "db/object.age", writtenAs(written))

	if err == nil {
		t.Fatal("bytes that differ at the same length should fail the run")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("the error should name the checksum, got %v", err)
	}
}

// An object that cannot be read back at all fails the run rather than passing on
// the assumption that the upload said it worked.
func TestVerifyObject_failsWhenTheObjectCannotBeRead(t *testing.T) {
	t.Parallel()

	want := errors.New("the bucket is unreachable")

	err := verifyObject(t.Context(), stubStore{err: want}, "db/object.age", writtenAs([]byte("x")))

	if !errors.Is(err, want) {
		t.Fatalf("want the fetch error wrapped, got %v", err)
	}
}

// covers: AC-14 - the read back failure is what reaches an alert path, so it
// must not carry the object key or the bucket with it.
func TestVerifyObject_theFailureDoesNotCarryTheObjectKey(t *testing.T) {
	t.Parallel()

	data := []byte("age-encryption.org/v1\nthe ciphertext")
	key := "db/20260815T030000Z-bkp_01M03FMYTH8XY161GVHW284VEK.age"

	err := verifyObject(t.Context(), stubStore{data: data[:2]}, key, writtenAs(data))

	if err == nil {
		t.Fatal("want a failure on a short read back")
	}
	if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "db/") {
		t.Errorf("the error carries the object key: %v", err)
	}
}
