// Package backup_test drives the whole thread against a real SQLite file in a
// temporary directory and a fake bucket in memory. Nothing about the snapshot,
// the checks, or the encryption is stubbed: the only thing standing in for the
// real world is the object store, which is an interface for exactly that reason.
package backup_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	_ "modernc.org/sqlite"

	"github.com/toyinogun/deployer/internal/backup"
	"github.com/toyinogun/deployer/internal/domain"
)

// fakeBucket is an in memory ObjectStore. It can be told to fail an upload, and
// to hand back fewer bytes than it was given, which is the case the read back
// exists to catch.
type fakeBucket struct {
	mu         sync.Mutex
	objects    map[string][]byte
	putErr     error
	truncateBy int
}

func newBucket() *fakeBucket { return &fakeBucket{objects: map[string][]byte{}} }

func (b *fakeBucket) Put(_ context.Context, key, path string, size int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.putErr != nil {
		return b.putErr
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return errors.New("the caller's size does not match the file")
	}
	b.objects[key] = data
	return nil
}

func (b *fakeBucket) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[key]
	if !ok {
		return nil, errors.New("no such object")
	}
	if b.truncateBy > 0 && len(data) > b.truncateBy {
		data = data[:len(data)-b.truncateBy]
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *fakeBucket) only(t *testing.T) (string, []byte) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.objects) != 1 {
		t.Fatalf("the bucket should hold exactly one object, holds %d", len(b.objects))
	}
	for k, v := range b.objects {
		return k, v
	}
	return "", nil
}

// memRuns is the record, in memory. It invents no store behaviour it does not
// have: it is the same small state machine the SQLite table enforces, and the
// table itself is proven in internal/store/backups_test.go.
type memRuns struct {
	mu     sync.Mutex
	rows   []backup.Run
	nextID int
	now    func() time.Time
	// startErr, when set, is what StartBackupRun returns instead of inserting.
	startErr error
}

func newRuns(now func() time.Time) *memRuns { return &memRuns{now: now} }

func (m *memRuns) StartBackupRun(_ context.Context, _ string) (backup.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.startErr != nil {
		return backup.Run{}, m.startErr
	}
	for _, r := range m.rows {
		if r.Outcome == "running" {
			return backup.Run{}, backup.ErrInFlight
		}
	}
	m.nextID++
	run := backup.Run{
		ID:        "bkp_" + string(rune('a'+m.nextID-1)),
		StartedAt: m.now(),
		Outcome:   "running",
	}
	m.rows = append(m.rows, run)
	return run, nil
}

func (m *memRuns) finish(id, outcome string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].ID == id && m.rows[i].Outcome == "running" {
			end := m.now()
			m.rows[i].Outcome = outcome
			m.rows[i].FinishedAt = &end
			return nil
		}
	}
	return errors.New("no running row")
}

func (m *memRuns) FinishBackupRunSucceeded(_ context.Context, id string, _ backup.Result) error {
	return m.finish(id, "succeeded")
}

func (m *memRuns) FinishBackupRunFailed(_ context.Context, id string, _ domain.BackupReason) error {
	return m.finish(id, "failed")
}

func (m *memRuns) latest(match func(string) bool) (backup.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.rows) - 1; i >= 0; i-- {
		if match(m.rows[i].Outcome) {
			return m.rows[i], nil
		}
	}
	return backup.Run{}, backup.ErrNoRun
}

func (m *memRuns) RunningBackupRun(context.Context) (backup.Run, error) {
	return m.latest(func(o string) bool { return o == "running" })
}

func (m *memRuns) LatestSucceededBackupRun(context.Context) (backup.Run, error) {
	return m.latest(func(o string) bool { return o == "succeeded" })
}

func (m *memRuns) LatestTerminalBackupRun(context.Context) (backup.Run, error) {
	return m.latest(func(o string) bool { return o == "succeeded" || o == "failed" })
}

func (m *memRuns) StrandBackupRuns(context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for i := range m.rows {
		if m.rows[i].Outcome == "running" {
			end := m.now()
			m.rows[i].Outcome = "failed"
			m.rows[i].FinishedAt = &end
			n++
		}
	}
	return n, nil
}

// countingAlerter records what it was asked to send.
type countingAlerter struct {
	failures  []domain.BackupReason
	recovered int
}

func (a *countingAlerter) BackupFailed(_ context.Context, r domain.BackupReason) {
	a.failures = append(a.failures, r)
}
func (a *countingAlerter) BackupRecovered(context.Context) { a.recovered++ }

// liveDB opens a real database with the one table the checks look at, holding
// the one row they insist on.
func liveDB(t *testing.T, accounts int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatalf("opening the live database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE accounts (id TEXT PRIMARY KEY, email TEXT) STRICT`); err != nil {
		t.Fatalf("creating accounts: %v", err)
	}
	for i := range accounts {
		if _, err := db.Exec(`INSERT INTO accounts (id, email) VALUES (?, ?)`,
			"acc_"+string(rune('a'+i)), "someone@example.com"); err != nil {
			t.Fatalf("inserting an account: %v", err)
		}
	}
	return db
}

type harness struct {
	svc      *backup.Service
	runs     *memRuns
	bucket   *fakeBucket
	alerter  *countingAlerter
	identity *age.X25519Identity
	tempDir  string
}

func newHarness(t *testing.T, accounts int) *harness {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generating a test identity: %v", err)
	}
	now := func() time.Time { return time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC) }
	h := &harness{
		runs:     newRuns(now),
		bucket:   newBucket(),
		alerter:  &countingAlerter{},
		identity: id,
		tempDir:  t.TempDir(),
	}
	h.svc = backup.New(h.runs, h.bucket, h.alerter, backup.Options{
		DB:        liveDB(t, accounts),
		Recipient: id.Recipient(),
		TempDir:   h.tempDir,
		Interval:  time.Hour,
		Now:       now,
	})
	if h.svc == nil {
		t.Fatal("the service should be configured")
	}
	return h
}

// leftovers is what is still sitting in the temp directory.
func (h *harness) leftovers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(h.tempDir)
	if err != nil {
		t.Fatalf("reading the temp directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// The whole thread: a real snapshot of a real database, checked, encrypted,
// uploaded, read back, and closed as a row. The object decrypts with the
// matching identity and opens as a database whose accounts match the source
// (AC-1, AC-3, AC-4, AC-5, AC-6, AC-7, AC-23 round trip).
func TestRunEndToEnd(t *testing.T) {
	h := newHarness(t, 2)

	reason, err := h.svc.Run(t.Context(), "")
	if err != nil || reason != "" {
		t.Fatalf("the run should have succeeded: reason %q, err %v", reason, err)
	}

	key, object := h.bucket.only(t)
	if got, want := key, "db/20260815T030000Z-bkp_a.age"; got != want {
		t.Fatalf("object key: got %q, want %q", got, want)
	}

	// The object is real age ciphertext, and the platform's own recipient is
	// the only thing that opens it.
	r, err := age.Decrypt(bytes.NewReader(object), h.identity)
	if err != nil {
		t.Fatalf("decrypting the object: %v", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading the plaintext: %v", err)
	}
	restored := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(restored, plain, 0o600); err != nil {
		t.Fatalf("writing the restored file: %v", err)
	}
	db, err := sql.Open("sqlite", restored)
	if err != nil {
		t.Fatalf("opening the restored database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var accounts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accounts); err != nil {
		t.Fatalf("counting accounts in the restored database: %v", err)
	}
	if accounts != 2 {
		t.Fatalf("the restored database should hold the source's 2 accounts, holds %d", accounts)
	}

	// The row closed succeeded, and neither temporary file survived (AC-2).
	last, err := h.runs.LatestSucceededBackupRun(t.Context())
	if err != nil {
		t.Fatalf("reading the newest success: %v", err)
	}
	if last.Outcome != "succeeded" {
		t.Fatalf("the row should be succeeded, is %q", last.Outcome)
	}
	if left := h.leftovers(t); len(left) != 0 {
		t.Fatalf("the temp directory should be empty, holds %v", left)
	}
	// A success after nothing is not a recovery, and it mails nobody (AC-13).
	if len(h.alerter.failures) != 0 || h.alerter.recovered != 0 {
		t.Fatalf("the first success should send nothing, sent %+v", h.alerter)
	}
}

// A read back that comes up short fails the run with the verify reason, leaves
// the object in place, and mails once (AC-6, AC-13).
func TestRunVerifyMismatch(t *testing.T) {
	h := newHarness(t, 1)
	h.bucket.truncateBy = 1

	reason, err := h.svc.Run(t.Context(), "")
	if reason != domain.BackupVerifyFailed {
		t.Fatalf("reason: got %q, want verify_failed (err %v)", reason, err)
	}
	if _, object := h.bucket.only(t); len(object) == 0 {
		t.Fatal("the object should be left in place")
	}
	if len(h.alerter.failures) != 1 || h.alerter.failures[0] != domain.BackupVerifyFailed {
		t.Fatalf("one failure mail carrying the reason, got %+v", h.alerter.failures)
	}
	if left := h.leftovers(t); len(left) != 0 {
		t.Fatalf("a failed run must clean up too, left %v", left)
	}
}

// A snapshot with no accounts in it uploads nothing at all (AC-3).
func TestRunEmptyDatabaseUploadsNothing(t *testing.T) {
	h := newHarness(t, 0)

	reason, _ := h.svc.Run(t.Context(), "")
	if reason != domain.BackupIntegrityFailed {
		t.Fatalf("reason: got %q, want integrity_failed", reason)
	}
	h.bucket.mu.Lock()
	defer h.bucket.mu.Unlock()
	if len(h.bucket.objects) != 0 {
		t.Fatalf("nothing should have been uploaded, %d objects landed", len(h.bucket.objects))
	}
}

// An upload that never lands fails with the upload reason and leaves nothing
// behind on the volume.
func TestRunUploadFailure(t *testing.T) {
	h := newHarness(t, 1)
	h.bucket.putErr = errors.New("the bucket is unreachable")

	reason, _ := h.svc.Run(t.Context(), "")
	if reason != domain.BackupUploadFailed {
		t.Fatalf("reason: got %q, want upload_failed", reason)
	}
	if left := h.leftovers(t); len(left) != 0 {
		t.Fatalf("the temp directory should be empty, holds %v", left)
	}
}

// A write fault that is not the unique index is internal and is alerted as such,
// rather than reading to an admin as a benign concurrency refusal (AC-8a).
func TestRunInsertFaultIsInternal(t *testing.T) {
	h := newHarness(t, 1)
	h.runs.startErr = errors.New("database is locked")

	reason, err := h.svc.Run(t.Context(), "")
	if reason != domain.BackupInternal || err == nil {
		t.Fatalf("reason: got %q (err %v), want internal", reason, err)
	}
	if len(h.alerter.failures) != 1 || h.alerter.failures[0] != domain.BackupInternal {
		t.Fatalf("an internal fault should alert as internal, got %+v", h.alerter.failures)
	}
}

// A run started while one is in flight is refused, and the refusal is its own
// answer rather than a failed run (AC-8, AC-20).
func TestRunRefusedWhileInFlight(t *testing.T) {
	h := newHarness(t, 1)
	if _, err := h.runs.StartBackupRun(t.Context(), ""); err != nil {
		t.Fatalf("occupying the index: %v", err)
	}

	reason, err := h.svc.Run(t.Context(), "acc_admin")
	if !errors.Is(err, backup.ErrInFlight) || reason != "" {
		t.Fatalf("got reason %q, err %v; want ErrInFlight and no reason", reason, err)
	}
	if len(h.alerter.failures) != 0 {
		t.Fatalf("a refusal is not a failure and mails nobody, got %+v", h.alerter.failures)
	}
}

// The first success after a failure mails once, and the success after that mails
// nothing, so silence means healthy (AC-13).
func TestRunRecoveryMail(t *testing.T) {
	h := newHarness(t, 1)
	h.bucket.putErr = errors.New("the bucket is unreachable")
	if reason, _ := h.svc.Run(t.Context(), ""); reason != domain.BackupUploadFailed {
		t.Fatalf("the first run should have failed, got %q", reason)
	}

	h.bucket.putErr = nil
	if reason, err := h.svc.Run(t.Context(), ""); reason != "" || err != nil {
		t.Fatalf("the second run should have succeeded: %q %v", reason, err)
	}
	if h.alerter.recovered != 1 {
		t.Fatalf("one recovery mail, got %d", h.alerter.recovered)
	}

	if reason, err := h.svc.Run(t.Context(), ""); reason != "" || err != nil {
		t.Fatalf("the third run should have succeeded: %q %v", reason, err)
	}
	if h.alerter.recovered != 1 {
		t.Fatalf("a success after a success mails nothing, recovery count is %d", h.alerter.recovered)
	}
}

// A process starting with files a killed predecessor left behind empties the
// directory before serving, and ends the row it left running (AC-2a, AC-9).
func TestSweepEmptiesTempDirAndStrandsRuns(t *testing.T) {
	h := newHarness(t, 1)
	if _, err := h.runs.StartBackupRun(t.Context(), ""); err != nil {
		t.Fatalf("leaving a running row: %v", err)
	}
	leftover := filepath.Join(h.tempDir, "bkp_dead.db")
	if err := os.WriteFile(leftover, []byte("every password hash on the platform"), 0o600); err != nil {
		t.Fatalf("leaving a plaintext file behind: %v", err)
	}

	backup.Sweep(t.Context(), h.runs, h.tempDir)

	if left := h.leftovers(t); len(left) != 0 {
		t.Fatalf("the sweep should have emptied the directory, holds %v", left)
	}
	if _, err := h.runs.RunningBackupRun(t.Context()); !errors.Is(err, backup.ErrNoRun) {
		t.Fatalf("the stranded row should be ended, got %v", err)
	}
}

// A tick arriving while a run is in flight skips without inserting, and the next
// one runs normally (AC-12a).
func TestScheduleSkipsWhileInFlight(t *testing.T) {
	h := newHarness(t, 1)
	occupying, err := h.runs.StartBackupRun(t.Context(), "")
	if err != nil {
		t.Fatalf("occupying the index: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		h.svc.Schedule(ctx)
		close(done)
	}()
	// The startup catch up fires immediately, and with a run in flight it must
	// skip rather than insert.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	h.bucket.mu.Lock()
	objects := len(h.bucket.objects)
	h.bucket.mu.Unlock()
	if objects != 0 {
		t.Fatalf("a skipped tick should upload nothing, %d objects landed", objects)
	}
	if err := h.runs.FinishBackupRunFailed(t.Context(), occupying.ID, domain.BackupInternal); err != nil {
		t.Fatalf("ending the occupying run: %v", err)
	}
	// With the index free the next run goes through, which is the catch up the
	// skip relies on.
	if reason, err := h.svc.Run(t.Context(), ""); reason != "" || err != nil {
		t.Fatalf("the next run should have succeeded: %q %v", reason, err)
	}
}

// An unconfigured platform has a nil service, and every method on it is safe.
func TestNilServiceIsSafe(t *testing.T) {
	var svc *backup.Service
	if svc.Configured() {
		t.Fatal("a nil service should not read as configured")
	}
	svc.Schedule(t.Context())
	if _, err := svc.Run(t.Context(), ""); err == nil {
		t.Fatal("running an unconfigured backup should be refused")
	}
}
