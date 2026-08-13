// Command deployer is the Deployer control plane: MCP server, upload endpoint,
// and deployment reconcile loop in one binary. See docs/specs/0001-stack-and-architecture.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/config"
	"github.com/toyinogun/deployer/internal/httpapi"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/kube"
	"github.com/toyinogun/deployer/internal/mail"
	"github.com/toyinogun/deployer/internal/mcp"
	"github.com/toyinogun/deployer/internal/reconcile"
	"github.com/toyinogun/deployer/internal/registry"
	"github.com/toyinogun/deployer/internal/store"
	"github.com/toyinogun/deployer/internal/uploads"
)

// dbRetryInterval is how long the boot waits between attempts to open and
// migrate the database. Short enough that a volume attaching late costs seconds,
// long enough that a genuinely broken volume does not spin the log.
const dbRetryInterval = 5 * time.Second

func main() {
	// One binary, two entry points. `fetch-source` runs as the build Job's init
	// container, so it loads none of the control plane's configuration and opens
	// no database: it reads the handful of variables the reconcile loop composed
	// onto it and nothing else (spec 0004, AC-8).
	if len(os.Args) > 1 && os.Args[1] == "fetch-source" {
		if err := fetchSource(context.Background(), os.Getenv); err != nil {
			slog.Error("fetching the source failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("shutting down", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h := &health{}
	defer h.close()

	mux := http.NewServeMux()
	// Liveness never touches the database: a locked or unwritable database must
	// take the pod out of its Service, not restart it in a loop (spec 0003, AC-4).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		respond(r.Context(), w, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := h.ready(r.Context()); err != nil {
			slog.WarnContext(r.Context(), "not ready", "error", err)
			respond(r.Context(), w, http.StatusServiceUnavailable, "not ready\n")
			return
		}
		respond(r.Context(), w, http.StatusOK, "ready\n")
	})

	// Everything under /v1 and the MCP endpoint need the database, which opens
	// behind the already bound server, so they are reached through the health
	// struct rather than wired in directly. Before the store is up they answer
	// 503, the same as readiness.
	realWork := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := h.api()
		if api == nil {
			respond(r.Context(), w, http.StatusServiceUnavailable, "not ready\n")
			return
		}
		api.ServeHTTP(w, r)
	})
	mux.Handle("/v1/", realWork)
	mux.Handle("/mcp", realWork)

	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// Recreate strategy means Kubernetes waits for this pod to exit before
	// starting the next one, so a clean shutdown is what keeps the swap quiet.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// A shutdown that times out means connections were still in flight. Log it
		// rather than dropping it: it is the difference between a clean pod swap
		// and one that cut a deploy off mid request.
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "error", err)
		}
	}()

	// The database opens behind the already bound server, so the probes can answer
	// while it is still coming up. Migrations still run before anything serves real
	// traffic, because readiness is what gates the Service (spec 0002, spec 0003).
	go h.open(ctx, cfg)

	slog.Info("deployer listening", "addr", cfg.Listen, "app_domain", cfg.AppDomain, "namespace", cfg.Namespace)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// respond writes a plain text probe response, logging a failed write rather than
// swallowing it.
func respond(ctx context.Context, w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	if _, err := w.Write([]byte(body)); err != nil {
		slog.WarnContext(ctx, "probe response write failed", "error", err)
	}
}

// health owns the database handle and answers the readiness probe. Opening
// happens in the background so a volume that is not writable yet leaves the pod
// not ready instead of crash looping (spec 0003, AC-4).
type health struct {
	mu    sync.RWMutex
	store *store.Store
	// routes is the /v1 surface, built once the store is open. Nil until then,
	// which is what makes those endpoints answer 503 rather than panic.
	routes *http.ServeMux
	err    error
}

// api returns the /v1 surface, or nil while the database is still opening.
func (h *health) api() http.Handler {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.routes == nil {
		return nil
	}
	return h.routes
}

// buildAPI wires the HTTP surface over an open store, and starts the reconcile
// loop behind it. Both are built here because both need the same open store and
// the same upload service.
//
// The loop is what makes a deploy happen: the tool surface only writes a queued
// row and waits on committed state (spec 0004, Key invariants).
func buildAPI(ctx context.Context, st *store.Store, cfg config.Config) *http.ServeMux {
	as := store.ForAuth(st)
	// One authenticator, two routes. The bearer route a machine uses and the
	// session route a person uses resolve through the same object, which is what
	// makes the verified and disabled gate impossible for a new surface to forget
	// (spec 0007, Key invariants).
	authenticator := auth.NewAuthenticator(as, as).WithSessions(as, identity.SessionLifetime)
	uploadSvc := uploads.NewService(store.ForUploads(st), cfg.UploadDir, cfg.MaxUploadBytes, nil)

	// A nil sender is a supported state: register, resend and password reset
	// answer mail_unavailable, and the whole MCP and upload path works normally
	// (spec 0007, AC-26).
	sender := mail.New(mail.Options{APIKey: cfg.ResendAPIKey, From: cfg.MailFrom})
	if sender == nil {
		slog.Warn("DEPLOYER_RESEND_API_KEY is unset, so nobody can register or reset a password")
	}
	identitySvc := identity.NewService(store.ForIdentity(st), mailerOrNil(sender), ids.SystemClock{},
		identity.Options{PublicURL: cfg.PublicURL})

	mux := http.NewServeMux()
	httpapi.New(authenticator, as, uploadSvc, cfg.MaxUploadBytes).Register(mux)
	httpapi.NewIdentity(identitySvc, authenticator, as, cfg.PublicURL, sender != nil).Register(mux)

	// One cluster client, shared by the loop that drives deploys and the tool
	// surface that reads an app's own output back. Nil when there is no in
	// cluster credential, which both sides handle rather than refusing to start.
	cluster, err := kube.New()
	if err != nil {
		slog.Warn("no Kubernetes access, so no deployment will be driven and no log can be read", "error", err)
		cluster = nil
	}

	tools := mcp.New(authenticator, as, store.ForMCPApps(st), store.ForMCPDeployments(st),
		forTool{svc: uploadSvc}, podReader(cluster), mcp.Options{
			PublicURL: cfg.PublicURL,
			AppDomain: cfg.AppDomain,
			// The registry pull credential is the one secret the platform placed
			// in the app's namespace itself, so it is the one it can redact
			// exactly (spec 0006, AC-6).
			SecretLiterals: []string{cfg.RegistryPass},
		})
	mux.Handle("/mcp", tools.Handler())

	if cluster != nil {
		startReconciler(ctx, st, cfg, uploadSvc, cluster)
	}
	return mux
}

// mailerOrNil keeps a nil sender nil through the interface, so the service sees
// an absent mailer rather than a non nil interface holding a nil pointer.
func mailerOrNil(s *mail.Sender) identity.Mailer {
	if s == nil {
		return nil
	}
	return s
}

// podReader keeps a nil client nil through the interface, so the tool sees an
// absent cluster rather than a non nil interface holding a nil pointer.
func podReader(cluster *kube.Client) mcp.Pods {
	if cluster == nil {
		return nil
	}
	return cluster
}

// startReconciler starts the deployment loop. The caller skips it when there is
// no cluster to drive, which is honest for a local run: an upload still works, a
// deploy stays queued.
func startReconciler(ctx context.Context, st *store.Store, cfg config.Config, uploadSvc *uploads.Service, cluster *kube.Client) {
	rs := store.ForReconcile(st)
	loop := reconcile.New(rs, rs, uploadSource{svc: uploadSvc},
		registry.New(cfg.RegistryHost, cfg.RegistryUser, cfg.RegistryPass),
		cluster, reconcile.Options{
			PodName:               cfg.PodName,
			ControlPlaneNamespace: cfg.Namespace,
			BuildNamespace:        cfg.BuildNamespace,
			AppDomain:             cfg.AppDomain,
			IngressClassName:      cfg.IngressClassName,
			SelfImage:             cfg.SelfImage,
			BuilderImage:          cfg.BuilderImage,
			BuildUID:              cfg.BuildUID,
			BuildGID:              cfg.BuildGID,
			InternalURL:           cfg.InternalURL,
			RegistryHost:          cfg.RegistryHost,
			RegistryUser:          cfg.RegistryUser,
			RegistryPass:          cfg.RegistryPass,
			DeployTimeout:         cfg.DeployTimeout,
			BuildTimeout:          cfg.BuildTimeout,
			ReadyTimeout:          cfg.ReadyTimeout,
			ReconcileInterval:     cfg.ReconcileInterval,
			MaxUploadFiles:        cfg.MaxUploadFiles,
			MaxExtractedBytes:     cfg.MaxExtractedBytes,
			CPU:                   cfg.AppCPU,
			Memory:                cfg.AppMemory,
			LimitCPU:              cfg.AppLimitCPU,
			LimitMemory:           cfg.AppLimitMemory,
			QuotaCPU:              cfg.AppQuotaCPU,
			QuotaMemory:           cfg.AppQuotaMemory,
			QuotaPods:             cfg.AppQuotaPods,
			EgressBlockedCIDRs:    cfg.AppEgressBlockedCIDRs,
		})
	go loop.Run(ctx)
	slog.Info("reconcile loop started", "interval", cfg.ReconcileInterval, "build_namespace", cfg.BuildNamespace)
}

// open keeps trying to open and migrate the database until it succeeds or the
// process is shutting down, recording the outcome for the readiness probe.
func (h *health) open(ctx context.Context, cfg config.Config) {
	for {
		st, err := openMigrated(ctx, cfg)
		h.mu.Lock()
		h.store, h.err = st, err
		if err == nil {
			h.routes = buildAPI(ctx, st, cfg)
		}
		h.mu.Unlock()
		if err == nil {
			slog.Info("database ready", "path", cfg.DBPath, "pod", cfg.PodName)
			return
		}
		slog.Error("the database is not ready, retrying", "error", err, "retry_in", dbRetryInterval)
		select {
		case <-ctx.Done():
			return
		case <-time.After(dbRetryInterval):
		}
	}
}

// openMigrated opens the database and brings it up to the latest migration.
func openMigrated(ctx context.Context, cfg config.Config) (*store.Store, error) {
	st, err := store.Open(store.Options{Path: cfg.DBPath, BusyTimeout: cfg.DBBusyTimeout})
	if err != nil {
		return nil, err
	}
	migrateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := st.Migrate(migrateCtx); err != nil {
		if closeErr := st.Close(); closeErr != nil {
			slog.Error("closing the database after a failed migration", "error", closeErr)
		}
		return nil, err
	}
	// Seeding runs behind the migration and before readiness, so the platform is
	// never reachable with a schema or an account it does not have yet.
	if err := seedBootstrap(migrateCtx, st, cfg); err != nil {
		if closeErr := st.Close(); closeErr != nil {
			slog.Error("closing the database after a failed bootstrap", "error", closeErr)
		}
		return nil, err
	}
	return st, nil
}

// seedBootstrap ensures the single account and token this slice authenticates
// with exist (spec 0004, AC-1). The raw token is passed straight through and
// never logged, at any level: only whether one was configured.
func seedBootstrap(ctx context.Context, st *store.Store, cfg config.Config) error {
	if cfg.BootstrapToken == "" {
		slog.Warn("DEPLOYER_BOOTSTRAP_TOKEN is unset, so the platform has no usable API token and every call will be refused")
		return nil
	}
	if err := auth.Bootstrap(ctx, store.ForAuth(st), cfg.BootstrapToken); err != nil {
		return err
	}
	slog.Info("bootstrap account seeded", "account", auth.BootstrapAccountName)
	return nil
}

// ready reports whether the control plane can take traffic.
func (h *health) ready(ctx context.Context) error {
	h.mu.RLock()
	st, err := h.store, h.err
	h.mu.RUnlock()
	if err != nil {
		return err
	}
	if st == nil {
		return errors.New("the database is still opening")
	}
	return st.Ready(ctx)
}

// close releases the database handle if one was ever opened.
func (h *health) close() {
	h.mu.RLock()
	st := h.store
	h.mu.RUnlock()
	if st == nil {
		return
	}
	if err := st.Close(); err != nil {
		slog.Error("closing the database failed", "error", err)
	}
}
