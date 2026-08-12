package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/toyinogun/deployer/internal/source"
)

// fetchTimeout bounds the whole fetch. The build Job has its own deadline; this
// is so a hung registry or a stalled connection fails as itself rather than as
// the build running out of time much later.
const fetchTimeout = 5 * time.Minute

// fetchSource is the `deployer fetch-source` subcommand, which runs as the build
// Job's init container rather than as part of the control plane.
//
// It redeems its single use token, verifies the platform's recorded SHA-256
// before unpacking anything, and hands a clean tree to the builder. Every input
// comes from the environment the reconcile loop composed, never from the
// archive: the expected hash in particular is the platform's record, not
// something the tarball carries.
func fetchSource(ctx context.Context, getenv func(string) string) error {
	url := getenv("DEPLOYER_FETCH_URL")
	token := getenv("DEPLOYER_FETCH_TOKEN")
	expected := getenv("DEPLOYER_EXPECTED_SHA256")
	dir := getenv("DEPLOYER_SOURCE_DIR")
	if url == "" || token == "" || expected == "" || dir == "" {
		return fmt.Errorf("fetch-source: DEPLOYER_FETCH_URL, DEPLOYER_FETCH_TOKEN, DEPLOYER_EXPECTED_SHA256 and DEPLOYER_SOURCE_DIR are all required")
	}
	limits, err := fetchLimits(getenv)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	archive, err := download(ctx, url, token, dir)
	if err != nil {
		return err
	}
	defer func() {
		// The archive is scratch. Leaving it behind would hand the builder a copy
		// of the source it has no reason to see twice.
		if err := os.Remove(archive); err != nil {
			fmt.Fprintf(os.Stderr, "fetch-source: removing the archive: %v\n", err)
		}
	}()

	if err := verify(archive, expected); err != nil {
		return err
	}

	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("fetch-source: reopening the archive: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := source.Extract(f, dir, limits); err != nil {
		return err
	}
	return nil
}

// fetchLimits reads the extraction caps the control plane passed down.
func fetchLimits(getenv func(string) string) (source.Limits, error) {
	files, err := strconv.Atoi(getenv("DEPLOYER_MAX_UPLOAD_FILES"))
	if err != nil || files <= 0 {
		return source.Limits{}, fmt.Errorf("fetch-source: DEPLOYER_MAX_UPLOAD_FILES must be a positive integer")
	}
	bytes, err := strconv.ParseInt(getenv("DEPLOYER_MAX_EXTRACTED_BYTES"), 10, 64)
	if err != nil || bytes <= 0 {
		return source.Limits{}, fmt.Errorf("fetch-source: DEPLOYER_MAX_EXTRACTED_BYTES must be a positive integer")
	}
	return source.Limits{MaxFiles: files, MaxBytes: bytes}, nil
}

// download spends the fetch token and writes the tarball beside the workspace.
func download(ctx context.Context, url, token, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("fetch-source: preparing the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch-source: fetching the source: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The status is the whole message. The body is the control plane's, and
		// this process writes to a log the builder's operator can read.
		return "", fmt.Errorf("fetch-source: the control plane answered %d", resp.StatusCode)
	}

	path := filepath.Join(filepath.Dir(dir), "source.tar.gz")
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("fetch-source: creating %s: %w", path, err)
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		return "", fmt.Errorf("fetch-source: writing %s: %w", path, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("fetch-source: closing %s: %w", path, closeErr)
	}
	return path, nil
}

// verify checks the archive against the hash the platform recorded at upload.
// It runs before a single entry is unpacked, so a tampered or truncated body
// never reaches the extractor, let alone the builder.
func verify(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("fetch-source: opening the archive: %w", err)
	}
	defer func() { _ = f.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return fmt.Errorf("fetch-source: hashing the archive: %w", err)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != expected {
		return fmt.Errorf("fetch-source: the archive does not match the recorded hash: %w", source.ErrRejected)
	}
	return nil
}
