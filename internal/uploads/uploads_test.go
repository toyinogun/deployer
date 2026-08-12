package uploads_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/uploads"
)

// fakeStore is the whole of uploads.Store, kept in memory. The real store is
// exercised in internal/store; what matters here is what the service hands it.
type fakeStore struct {
	rows    map[string]uploads.Upload // id -> upload
	byToken map[string]string         // fetch token hash -> upload id

	created   []uploads.New
	setTokens []string // upload ids whose token was replaced, in order

	createErr error
	setErr    error
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]uploads.Upload{}, byToken: map[string]string{}}
}

func (f *fakeStore) CreateUpload(_ context.Context, u uploads.New) (uploads.Upload, error) {
	if f.createErr != nil {
		return uploads.Upload{}, f.createErr
	}
	f.created = append(f.created, u)
	row := uploads.Upload{
		ID:        u.ID,
		AccountID: u.AccountID,
		Path:      u.Path,
		SizeBytes: u.SizeBytes,
		SHA256:    u.SHA256,
		ExpiresAt: u.ExpiresAt,
	}
	f.rows[u.ID] = row
	f.byToken[u.FetchTokenHash] = u.ID
	return row, nil
}

func (f *fakeStore) GetUpload(_ context.Context, id string) (uploads.Upload, error) {
	if row, ok := f.rows[id]; ok {
		return row, nil
	}
	return uploads.Upload{}, uploads.ErrNotFound
}

func (f *fakeStore) RedeemUpload(_ context.Context, hash string) (uploads.Upload, error) {
	id, ok := f.byToken[hash]
	if !ok {
		return uploads.Upload{}, uploads.ErrNotFound
	}
	row := f.rows[id]
	if row.Redeemed {
		return uploads.Upload{}, uploads.ErrRedeemed
	}
	row.Redeemed = true
	f.rows[id] = row
	return row, nil
}

func (f *fakeStore) SetFetchToken(_ context.Context, uploadID, hash string) error {
	if f.setErr != nil {
		return f.setErr
	}
	row, ok := f.rows[uploadID]
	if !ok {
		return uploads.ErrNotFound
	}
	f.setTokens = append(f.setTokens, uploadID)
	for existing, id := range f.byToken {
		if id == uploadID {
			delete(f.byToken, existing)
		}
	}
	f.byToken[hash] = uploadID
	row.Redeemed = false
	f.rows[uploadID] = row
	return nil
}

// gzipped returns a real gzip stream carrying body, which is the only kind of
// upload the service accepts.
func gzipped(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatalf("writing the gzip body: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the gzip writer: %v", err)
	}
	return buf.Bytes()
}

// harness is a service over a temp dir and a pinned clock, so the timestamps a
// test asserts are the ones it set.
type harness struct {
	svc   *uploads.Service
	store *fakeStore
	dir   string
	clock *ids.FixedClock
}

func newHarness(t *testing.T, maxBytes int64) harness {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "uploads")
	store := newFakeStore()
	clock := &ids.FixedClock{T: time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)}
	return harness{
		svc:   uploads.NewService(store, dir, maxBytes, clock),
		store: store,
		dir:   dir,
		clock: clock,
	}
}

func TestAcceptRecordsWhatLanded(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	h := newHarness(t, 1<<20)
	body := gzipped(t, "hello")

	up, err := h.svc.Accept(context.Background(), "acct_1", bytes.NewReader(body))

	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if up.AccountID != "acct_1" {
		t.Errorf("account = %q, want acct_1", up.AccountID)
	}
	if up.SizeBytes != int64(len(body)) {
		t.Errorf("size = %d, want %d", up.SizeBytes, len(body))
	}
	sum := sha256.Sum256(body)
	if up.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want the hash of the bytes sent", up.SHA256)
	}
	if _, err := os.Stat(up.Path); err != nil {
		t.Errorf("the file did not land at %q: %v", up.Path, err)
	}
}

func TestAcceptPutsTheFileWhereThePlatformChoseAndNowhereElse(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	h := newHarness(t, 1<<20)

	up, err := h.svc.Accept(context.Background(), "acct_1", bytes.NewReader(gzipped(t, "hello")))

	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if want := filepath.Join(h.dir, up.ID); up.Path != want {
		t.Errorf("path = %q, want the upload dir joined with the row's own id (%q)", up.Path, want)
	}
	if !strings.HasPrefix(up.ID, "upl_") {
		t.Errorf("id = %q, want a platform generated upl_ id", up.ID)
	}
}

func TestAcceptExpiresExactlyOneWindowAfterTheClockItRead(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	h := newHarness(t, 1<<20)

	up, err := h.svc.Accept(context.Background(), "acct_1", bytes.NewReader(gzipped(t, "hello")))

	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if want := ids.Stamp(h.clock.T.Add(uploads.Window)); up.ExpiresAt != want {
		t.Errorf("expires_at = %q, want %q", up.ExpiresAt, want)
	}
}

func TestAcceptSeedsAFetchTokenNoCallerCanConstruct(t *testing.T) {
	// covers: AC-2, AC-8
	t.Parallel()
	h := newHarness(t, 1<<20)
	ctx := context.Background()

	up, err := h.svc.Accept(ctx, "acct_1", bytes.NewReader(gzipped(t, "hello")))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	// The placeholder is the hash of a random value that was thrown away unread,
	// so nothing derivable from the upload unlocks it.
	seeded := h.store.created[0].FetchTokenHash
	if seeded == "" {
		t.Fatal("no fetch token hash was seeded, but the column is NOT NULL")
	}
	for _, guess := range []string{up.ID, up.SHA256, up.Path, "acct_1", ""} {
		if _, err := h.svc.Redeem(ctx, guess); !errors.Is(err, uploads.ErrNotFound) {
			t.Errorf("Redeem(%q) = %v, want ErrNotFound", guess, err)
		}
	}
}

func TestAcceptGeneratesAFreshIDPerUpload(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	h := newHarness(t, 1<<20)
	ctx := context.Background()

	first, err := h.svc.Accept(ctx, "acct_1", bytes.NewReader(gzipped(t, "one")))
	if err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	second, err := h.svc.Accept(ctx, "acct_1", bytes.NewReader(gzipped(t, "two")))
	if err != nil {
		t.Fatalf("second Accept: %v", err)
	}

	if first.ID == second.ID {
		t.Errorf("both uploads got id %q", first.ID)
	}
	if first.Path == second.Path {
		t.Errorf("both uploads landed on %q, so one overwrote the other", first.Path)
	}
}

func TestAcceptRefusesABodyOverTheCapAndLeavesNothingBehind(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	h := newHarness(t, 64)
	// A real gzip stream, padded past the cap, so it is the size that refuses it
	// rather than the format check.
	body := append(gzipped(t, "hello"), bytes.Repeat([]byte("x"), 4096)...)

	_, err := h.svc.Accept(context.Background(), "acct_1", bytes.NewReader(body))

	if !errors.Is(err, uploads.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	assertDirEmpty(t, h.dir)
	if len(h.store.created) != 0 {
		t.Error("an oversized body was still recorded in the store")
	}
}

func TestAcceptAcceptsABodyExactlyAtTheCap(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	body := gzipped(t, "hello")
	h := newHarness(t, int64(len(body)))

	up, err := h.svc.Accept(context.Background(), "acct_1", bytes.NewReader(body))

	if err != nil {
		t.Fatalf("a body exactly at the cap was refused: %v", err)
	}
	if up.SizeBytes != int64(len(body)) {
		t.Errorf("size = %d, want %d", up.SizeBytes, len(body))
	}
}

func TestAcceptRefusesSomethingThatWasNeverGzip(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	tests := []struct {
		name string
		body []byte
	}{
		{"plain text", []byte("this is not a tarball")},
		{"an empty body", nil},
		{"a single byte", []byte{0x1f}},
		{"the gzip magic byte followed by the wrong one", []byte{0x1f, 0x00, 0x01}},
		{"a zip archive", []byte("PK\x03\x04rest")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, 1<<20)

			_, err := h.svc.Accept(context.Background(), "acct_1", bytes.NewReader(tt.body))

			if !errors.Is(err, uploads.ErrNotGzip) {
				t.Fatalf("err = %v, want ErrNotGzip", err)
			}
			assertDirEmpty(t, h.dir)
		})
	}
}

func TestAcceptDiscardsTheFileWhenTheStoreRefusesTheRow(t *testing.T) {
	// covers: AC-2, AC-22
	t.Parallel()
	h := newHarness(t, 1<<20)
	h.store.createErr = errors.New("database is locked")

	_, err := h.svc.Accept(context.Background(), "acct_1", bytes.NewReader(gzipped(t, "hello")))

	if err == nil {
		t.Fatal("want an error")
	}
	assertDirEmpty(t, h.dir)
}

func TestRedeemSpendsTheTokenExactlyOnce(t *testing.T) {
	// covers: AC-8
	t.Parallel()
	h := newHarness(t, 1<<20)
	ctx := context.Background()
	up, err := h.svc.Accept(ctx, "acct_1", bytes.NewReader(gzipped(t, "hello")))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	token, err := h.svc.MintFetchToken(ctx, up.ID)
	if err != nil {
		t.Fatalf("MintFetchToken: %v", err)
	}

	redeemed, err := h.svc.Redeem(ctx, token)
	if err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	if redeemed.ID != up.ID {
		t.Errorf("redeemed %q, want %q", redeemed.ID, up.ID)
	}

	if _, err := h.svc.Redeem(ctx, token); !errors.Is(err, uploads.ErrRedeemed) {
		t.Errorf("replaying a spent token = %v, want ErrRedeemed", err)
	}
}

func TestRedeemRefusesATokenItNeverMinted(t *testing.T) {
	// covers: AC-8
	t.Parallel()
	h := newHarness(t, 1<<20)
	ctx := context.Background()
	if _, err := h.svc.Accept(ctx, "acct_1", bytes.NewReader(gzipped(t, "hello"))); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	for _, forged := range []string{"", "forged", strings.Repeat("a", 64)} {
		if _, err := h.svc.Redeem(ctx, forged); !errors.Is(err, uploads.ErrNotFound) {
			t.Errorf("Redeem(%q) = %v, want ErrNotFound", forged, err)
		}
	}
}

func TestMintFetchTokenGivesAResumedBuildAWorkingTokenAndKillsTheOld(t *testing.T) {
	// covers: AC-8, AC-18
	t.Parallel()
	h := newHarness(t, 1<<20)
	ctx := context.Background()
	up, err := h.svc.Accept(ctx, "acct_1", bytes.NewReader(gzipped(t, "hello")))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	first, err := h.svc.MintFetchToken(ctx, up.ID)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	if _, err := h.svc.Redeem(ctx, first); err != nil {
		t.Fatalf("redeeming the first token: %v", err)
	}

	second, err := h.svc.MintFetchToken(ctx, up.ID)
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if second == first {
		t.Fatal("the second mint returned the same token, so a leaked one stays live")
	}
	if _, err := h.svc.Redeem(ctx, second); err != nil {
		t.Errorf("the freshly minted token did not work: %v", err)
	}
	if _, err := h.svc.Redeem(ctx, first); !errors.Is(err, uploads.ErrNotFound) {
		t.Errorf("the retired token = %v, want ErrNotFound", err)
	}
}

func TestMintFetchTokenStoresOnlyTheHash(t *testing.T) {
	// covers: AC-8
	t.Parallel()
	h := newHarness(t, 1<<20)
	ctx := context.Background()
	up, err := h.svc.Accept(ctx, "acct_1", bytes.NewReader(gzipped(t, "hello")))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	token, err := h.svc.MintFetchToken(ctx, up.ID)
	if err != nil {
		t.Fatalf("MintFetchToken: %v", err)
	}

	sum := sha256.Sum256([]byte(token))
	stored, ok := h.store.byToken[hex.EncodeToString(sum[:])]
	if !ok {
		t.Fatal("the store does not hold the SHA-256 of the minted token")
	}
	if stored != up.ID {
		t.Errorf("the hash maps to %q, want %q", stored, up.ID)
	}
	if _, raw := h.store.byToken[token]; raw {
		t.Error("the raw fetch token was stored")
	}
}

func TestMintFetchTokenReportsAStoreFailure(t *testing.T) {
	// covers: AC-8
	t.Parallel()
	h := newHarness(t, 1<<20)
	ctx := context.Background()
	up, err := h.svc.Accept(ctx, "acct_1", bytes.NewReader(gzipped(t, "hello")))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	h.store.setErr = errors.New("database is locked")

	token, err := h.svc.MintFetchToken(ctx, up.ID)

	if err == nil {
		t.Fatal("want an error")
	}
	if token != "" {
		t.Errorf("token = %q, want empty when the hash was never recorded", token)
	}
}

func TestRemoveClearsTheTarballOnceTheDeployIsTerminal(t *testing.T) {
	// covers: AC-22
	t.Parallel()
	h := newHarness(t, 1<<20)
	ctx := context.Background()
	up, err := h.svc.Accept(ctx, "acct_1", bytes.NewReader(gzipped(t, "hello")))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	h.svc.Remove(ctx, up.Path)

	assertDirEmpty(t, h.dir)
}

func TestRemoveIsSafeToCallTwiceOrWithNoPath(t *testing.T) {
	// covers: AC-22
	t.Parallel()
	h := newHarness(t, 1<<20)
	ctx := context.Background()
	up, err := h.svc.Accept(ctx, "acct_1", bytes.NewReader(gzipped(t, "hello")))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	// A resumed reconcile loop can reach the terminal branch more than once.
	h.svc.Remove(ctx, up.Path)
	h.svc.Remove(ctx, up.Path)
	h.svc.Remove(ctx, "")
	h.svc.Remove(ctx, filepath.Join(h.dir, "upl_never_existed"))

	assertDirEmpty(t, h.dir)
}

func TestGetReadsBackTheRecordedUpload(t *testing.T) {
	// covers: AC-2
	t.Parallel()
	h := newHarness(t, 1<<20)
	ctx := context.Background()
	up, err := h.svc.Accept(ctx, "acct_1", bytes.NewReader(gzipped(t, "hello")))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	got, err := h.svc.Get(ctx, up.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != up.ID || got.SHA256 != up.SHA256 {
		t.Errorf("Get returned %+v, want %+v", got, up)
	}

	if _, err := h.svc.Get(ctx, "upl_unknown"); !errors.Is(err, uploads.ErrNotFound) {
		t.Errorf("Get on an unknown id = %v, want ErrNotFound", err)
	}
}

// assertDirEmpty fails when anything is left on the upload volume, which is what
// AC-22 asks for after a terminal deploy and what a refused body must never
// leave behind.
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s still holds %v, want nothing", dir, names)
	}
}
