// Package main's tests cover the probe surface the cluster depends on: the
// readiness gate that takes the pod out of its Service, and the liveness probe
// that must never do so. Nothing is mocked; the database is a real SQLite file
// in a temporary directory, and the server tests drive the real run().
package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/config"
)

// --- respond -----------------------------------------------------------------

func TestRespond_writesTheStatusAndBody(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()

	respond(t.Context(), rec, http.StatusServiceUnavailable, "not ready\n")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: want %d, got %d", http.StatusServiceUnavailable, rec.Code)
	}
	if got := rec.Body.String(); got != "not ready\n" {
		t.Errorf("body: want %q, got %q", "not ready\n", got)
	}
}

// failingWriter is a ResponseWriter whose body write always fails, standing in
// for a client that hung up mid response.
type failingWriter struct {
	header http.Header
	status int
}

func (w *failingWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *failingWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }
func (w *failingWriter) WriteHeader(status int)    { w.status = status }

func TestRespond_stillSetsTheStatusWhenTheWriteFails(t *testing.T) {
	t.Parallel()

	w := &failingWriter{}

	respond(t.Context(), w, http.StatusOK, "ok\n")

	// The status is what the kubelet reads, so a failed body write must not cost
	// it: respond logs the error rather than swallowing or panicking on it.
	if w.status != http.StatusOK {
		t.Errorf("status: want %d, got %d", http.StatusOK, w.status)
	}
}

// --- health ------------------------------------------------------------------

// testConfig returns a config pointing at a fresh database file under dir.
func testConfig(dir string) config.Config {
	return config.Config{
		DBPath:        filepath.Join(dir, "deployer.db"),
		DBBusyTimeout: 5 * time.Second,
		PodName:       "deployer-test-0",
	}
}

// covers: AC-4 - before the database is open the pod is not ready, rather than
// reporting ready and taking traffic it cannot serve.
func TestHealthReady_notReadyBeforeTheDatabaseOpens(t *testing.T) {
	t.Parallel()

	h := &health{}

	err := h.ready(t.Context())

	if err == nil {
		t.Fatal("want an error while the database is still opening, got nil")
	}
}

// covers: AC-4 - a failed open surfaces on readiness with its own reason.
func TestHealthReady_reportsTheOpenError(t *testing.T) {
	t.Parallel()

	want := errors.New("the volume is not writable")
	h := &health{err: want}

	err := h.ready(t.Context())

	if !errors.Is(err, want) {
		t.Fatalf("want the recorded open error, got %v", err)
	}
}

// covers: AC-4 - ready only once the database is open and every migration has
// applied.
func TestHealthReady_okOnceTheDatabaseIsOpenAndMigrated(t *testing.T) {
	t.Parallel()

	h := &health{}
	h.open(t.Context(), testConfig(t.TempDir()))
	t.Cleanup(h.close)

	if err := h.ready(t.Context()); err != nil {
		t.Fatalf("want ready after a successful open, got %v", err)
	}
}

// covers: AC-4 - an unusable volume leaves the pod not ready and open keeps
// retrying until shutdown, instead of returning and leaving it wedged, and
// instead of exiting into a restart loop.
func TestHealthOpen_keepsRetryingWhileTheDatabaseIsUnusable(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t.TempDir())
	// A directory that does not exist: SQLite cannot create the file there, so
	// every attempt fails the way an unmounted or unwritable volume would.
	cfg.DBPath = filepath.Join(t.TempDir(), "missing", "deployer.db")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h := &health{}
		h.open(ctx, cfg)
		if err := h.ready(context.Background()); err == nil {
			t.Error("want not ready with an unusable database, got ready")
		}
	}()

	// Give the first attempt time to fail and be recorded, then shut down.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(dbRetryInterval + 5*time.Second):
		t.Fatal("open did not return after the context was cancelled")
	}
}

func TestHealthClose_isSafeWhenNothingWasEverOpened(t *testing.T) {
	t.Parallel()

	h := &health{}

	h.close() // must not panic on a nil store
}

// --- openMigrated ------------------------------------------------------------

func TestOpenMigrated_returnsAFullyMigratedStore(t *testing.T) {
	t.Parallel()

	st, err := openMigrated(t.Context(), testConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("openMigrated: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})

	// Ready is exactly what the probe asks, and it fails while a migration is
	// pending, so a nil here means the schema is fully applied.
	if err := st.Ready(t.Context()); err != nil {
		t.Fatalf("want no pending migrations, got %v", err)
	}
}

func TestOpenMigrated_errorsOnAnUnwritablePath(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t.TempDir())
	cfg.DBPath = filepath.Join(t.TempDir(), "missing", "deployer.db")

	st, err := openMigrated(t.Context(), cfg)

	if err == nil {
		if closeErr := st.Close(); closeErr != nil {
			t.Errorf("closing the store: %v", closeErr)
		}
		t.Fatal("want an error for a path that cannot be created, got nil")
	}
}

// --- the running server ------------------------------------------------------

// freeAddr reserves a loopback port, releases it, and returns the address, so
// the server under test binds somewhere the machine is not already using.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}

// startServer boots the real run() against dbPath and returns its address. It
// stops the server with SIGTERM at the end of the test, the same signal the
// kubelet sends, so the graceful shutdown path is exercised too.
func startServer(t *testing.T, dbPath string) string {
	t.Helper()

	addr := freeAddr(t)
	t.Setenv("DEPLOYER_LISTEN", addr)
	t.Setenv("DEPLOYER_DB_PATH", dbPath)
	t.Setenv("DEPLOYER_REGISTRY_HOST", "registry.deployer-system.svc:5000")
	t.Setenv("DEPLOYER_REGISTRY_USER", "deployer")
	t.Setenv("DEPLOYER_REGISTRY_PASSWORD", "not-a-real-password")
	t.Setenv("DEPLOYER_APP_DOMAIN", "deploy.example.test")
	t.Setenv("DEPLOYER_NAMESPACE", "deployer-system")
	t.Setenv("DEPLOYER_POD_NAME", "deployer-test-0")
	t.Setenv("DEPLOYER_PUBLIC_URL", "https://deployer.example.test")
	t.Setenv("DEPLOYER_BUILDER_IMAGE", "paketobuildpacks/builder-jammy-base@sha256:"+strings.Repeat("a", 64))
	t.Setenv("DEPLOYER_SELF_IMAGE", "ghcr.io/toyinogun/deployer@sha256:"+strings.Repeat("b", 64))

	done := make(chan error, 1)
	go func() { done <- run() }()

	waitForProbe(t, addr)

	t.Cleanup(func() {
		// run() installed the SIGTERM handler before it began listening, and the
		// probe above proved it is listening, so this is caught rather than
		// killing the test binary.
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Errorf("signalling the server: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run returned an error: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("run did not return after SIGTERM")
		}
	})

	return addr
}

// waitForProbe blocks until the server answers, or fails the test.
func waitForProbe(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := get(t, addr, "/healthz"); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the server never answered on %s", addr)
}

// get performs one probe request and returns its status code.
func get(t *testing.T, addr, path string) (int, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode, nil
}

// awaitStatus polls path until it answers code, then reports the last status it
// saw so a failure names what the probe was actually returning.
func awaitStatus(t *testing.T, addr, path string, within time.Duration, code int) int {
	t.Helper()
	deadline := time.Now().Add(within)
	last := 0
	for time.Now().Before(deadline) {
		got, err := get(t, addr, path)
		if err == nil {
			last = got
			if got == code {
				return got
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return last
}

// covers: AC-4 - with a healthy database the control plane reports ready, so it
// is admitted to its Service.
func TestRun_readyOnceTheDatabaseIsMigrated(t *testing.T) {
	addr := startServer(t, filepath.Join(t.TempDir(), "deployer.db"))

	if got := awaitStatus(t, addr, "/readyz", 15*time.Second, http.StatusOK); got != http.StatusOK {
		t.Fatalf("/readyz: want %d, got %d", http.StatusOK, got)
	}
}

// covers: AC-4 - with an unusable database readiness fails (the pod leaves its
// Service) while liveness keeps answering 200 (the pod is not restarted in a
// loop). This is the whole point of splitting the two probes.
func TestRun_livenessStaysOkWhileReadinessFailsOnABrokenDatabase(t *testing.T) {
	addr := startServer(t, filepath.Join(t.TempDir(), "missing", "deployer.db"))

	if got := awaitStatus(t, addr, "/readyz", 5*time.Second, http.StatusServiceUnavailable); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz: want %d with an unusable database, got %d", http.StatusServiceUnavailable, got)
	}

	// Asked repeatedly, because a liveness probe that fails even once in a while
	// is what produces the restart loop AC-4 rules out.
	for i := range 5 {
		got, err := get(t, addr, "/healthz")
		if err != nil {
			t.Fatalf("/healthz attempt %d: %v", i+1, err)
		}
		if got != http.StatusOK {
			t.Fatalf("/healthz attempt %d: want %d, got %d", i+1, http.StatusOK, got)
		}
	}
}

// The probes are registered on the method and path, so anything else is a 404
// and no other surface is accidentally exposed on the platform's own port.
func TestRun_servesNothingBesidesTheProbes(t *testing.T) {
	addr := startServer(t, filepath.Join(t.TempDir(), "deployer.db"))

	got, err := get(t, addr, "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if got != http.StatusNotFound {
		t.Fatalf("GET /: want %d, got %d", http.StatusNotFound, got)
	}
}
