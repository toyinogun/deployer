// Package web is the browser surface: server rendered pages over the same
// identity service and store methods the JSON and MCP surfaces already call.
//
// It is an edge package, in the same position as internal/httpapi. It may reach
// the store and the services; nothing inward may reach it. Every page is a thin
// wrapper: no page holds a rule of its own, and no page queries through a path
// no other surface uses, because a rule that differs between the browser and the
// agent is a rule that is wrong on one of them (spec 0013, AC-4).
package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/identity"
	"github.com/toyinogun/deployer/internal/logs"
	"github.com/toyinogun/deployer/internal/store"
	"github.com/toyinogun/deployer/internal/suspend"
)

// Data is every read a page makes. It is declared here, where it is used, and
// the real store satisfies it: a page never gets a query written for it, so a
// method appearing here that no other surface calls is the signal the layering
// broke (spec 0013, Key invariants).
type Data interface {
	ListAppSummaryPage(ctx context.Context, accountID string, page store.Page) ([]store.AppSummary, error)
	GetAppBySlug(ctx context.Context, slug string) (store.App, error)
	GetRelease(ctx context.Context, id string) (store.Release, error)
	GetLatestDeploymentForApp(ctx context.Context, appID string) (store.Deployment, error)
	ListReleasesByApp(ctx context.Context, appID string, page store.Page) ([]store.Release, error)
	ListConfigForResponse(ctx context.Context, appID string) ([]store.ConfigEntry, error)
	CurrentReleaseConfig(ctx context.Context, appID string) (map[string]string, error)
	// CountLiveAppsByAccount is the same live count the deploy refusal is
	// decided from, so the number a page shows and the number a tool enforces
	// can never disagree (spec 0016, AC-10).
	CountLiveAppsByAccount(ctx context.Context, accountID string) (int, error)
	// CountLiveAppsPerAccount is that count for every account, in one grouped
	// statement, for the admin listing (spec 0016, AC-12).
	CountLiveAppsPerAccount(ctx context.Context) (map[string]int, error)
}

// Pods is the app output read, at the moment of the request. The same pair the
// get_logs tool reads through, so the browser cannot see a line the agent
// cannot (spec 0013, AC-19).
type Pods interface {
	PodsForApp(ctx context.Context, namespace, slug string) ([]logs.PodStatus, error)
	PodLog(ctx context.Context, namespace, pod string, tailLines int, previous bool) (string, error)
}

// Options is what the pages need from configuration. Every value is one an
// existing surface already validated at startup.
type Options struct {
	// MCPURL is the deploy path's public base address, derived from
	// DEPLOYER_MCP_HOST. It is the base of the two endpoints the onboarding
	// panel shows, and nothing else: the pages themselves are never served
	// there (spec 0022, AC-9).
	MCPURL string
	// AppDomain is the wildcard domain apps are served under, joined with a
	// slug to make the hostname a page shows.
	AppDomain string
	// CSRFKey is the secret each form's synchroniser token is derived under.
	CSRFKey []byte
	// SecretLiterals is what the platform itself placed in an app's namespace,
	// redacted out of that app's output exactly as get_logs redacts it.
	SecretLiterals []string
	// HasMailer is whether a sender is configured. The pages that exist only to
	// send mail say so plainly when it is not.
	HasMailer bool
	// MaxAppsPerAccount is how many live apps one account may hold, the same
	// number deploy_app is refused against (spec 0016, AC-10, AC-11).
	MaxAppsPerAccount int
	// ConsoleHost is the public console hostname. Every public route is
	// registered a second time under it and everything else on that host answers
	// 404, which is what keeps the deploy path and the admin surface off the open
	// internet (spec 0021, AC-2, AC-2a).
	ConsoleHost string
	// ConsoleURL is the console's own base address. It is the origin a page POST
	// is compared against, and the address the session cookie's Secure flag is
	// derived from: the cookie belongs to this surface, so it follows this
	// surface's own address rather than the machine surface's (spec 0021,
	// AC-21; spec 0022, AC-9).
	ConsoleURL string
	// TrustedHosts are the platform's public hostnames, the ones
	// CF-Connecting-IP is read on. The same set every other surface holds, so
	// one visitor is one address rather than one per surface (spec 0022, AC-14).
	TrustedHosts []string
	// ConnectorLimiter is the bucket the three OAuth endpoints spend from. It is
	// a dependency rather than a value, and it lives here beside the rest of
	// what this surface is handed, the same way internal/mcp carries its own. It
	// must never be the sign in limiter: adding one connector spends it three
	// times in a row from one address, so sharing that bucket would let a person
	// adding a connector lock themselves out of the console they are signing in
	// to (spec 0024, AC-6, AC-22). Nil means unlimited, which is a test.
	ConnectorLimiter *identity.Limiter
}

// Server is the page surface.
type Server struct {
	svc     *identity.Service
	auth    *auth.Authenticator
	auditor auth.Auditor
	data    Data
	pods    Pods
	// suspension is the one implementation of stopping an account and everything
	// it runs. The JSON admin route holds the same one, which is what keeps the
	// two surfaces from drifting into two meanings of suspended (spec 0018,
	// AC-19).
	suspension *suspend.Service
	// backups is the backup service, nil when the platform is not configured for
	// them. The page renders that as its own state rather than as an error
	// (spec 0020, AC-18).
	backups    Backups
	backupRuns BackupRuns
	opts       Options

	// origins is the configured half of the addresses a POST may claim to come
	// from, reduced to scheme and host and precomputed because every POST
	// compares against it. The console's own address is the only one that can be
	// known ahead of a request; the pages also answer on the bare pattern, so
	// every other name they serve, the tailnet and the LAN included, is accepted
	// by the same origin comparison in acceptedOrigin rather than from this list
	// (spec 0021, AC-21).
	origins []string
	// secure is the cookie's Secure flag, derived from ConsoleURL exactly as the
	// JSON surface derives it, so the two cannot hand out different cookies.
	secure bool
	// connectors is the OAuth endpoints' own bucket, lifted out of Options so
	// every spend site reads the same field.
	connectors *identity.Limiter
}

// New returns the page surface. data is the store, pods may be nil when there is
// no cluster credential, which the logs page renders as an empty state rather
// than refusing to start.
func New(svc *identity.Service, a *auth.Authenticator, auditor auth.Auditor, data Data, pods Pods,
	suspension *suspend.Service, backups Backups, backupRuns BackupRuns, opts Options,
) *Server {
	s := &Server{svc: svc, auth: a, auditor: auditor, data: data, pods: pods, suspension: suspension,
		backups: backups, backupRuns: backupRuns, opts: opts, connectors: opts.ConnectorLimiter}
	if u, err := url.Parse(opts.ConsoleURL); err == nil && u.Host != "" {
		s.origins = append(s.origins, u.Scheme+"://"+u.Host)
		s.secure = u.Scheme == "https"
	}
	return s
}

// Register adds every page route to mux. The paths are the root ones: /v1 and
// /mcp are untouched by this package.
//
// Every public route is registered twice, once bare and once prefixed with the
// console host, and one `<console host>/` pattern catches everything else on
// that host and answers 404. The mux's own most specific match rule then decides,
// so there is one routing table rather than a table plus a classifier that can
// drift from it (spec 0021, AC-2a).
//
// The direction this fails in is the safe one. A future route nobody remembers
// to register twice is private, because registration is opt in: it answers on the
// tailnet and 404s on the console. The admin routes below are the same mechanism
// used on purpose, registered once and so absent from the console entirely.
//
// The method is part of the doubling, not decoration. A pattern registered as
// `GET <console>/login` leaves `POST <console>/login` to the catch all, which
// would 404 the sign in form on the one host it exists to serve.
func (s *Server) Register(mux *http.ServeMux) {
	// public is every route the open internet may reach. Each is registered on
	// both hosts by the loop below.
	public := []struct {
		pattern string
		handler http.Handler
	}{
		{"GET /{$}", http.HandlerFunc(s.root)},
		{"GET /static/", http.StripPrefix("/static/", staticHandler())},

		{"GET /login", http.HandlerFunc(s.loginPage)},
		{"POST /login", http.HandlerFunc(s.loginSubmit)},
		{"GET /register", http.HandlerFunc(s.registerPage)},
		{"POST /register", http.HandlerFunc(s.registerSubmit)},
		{"GET /verify", http.HandlerFunc(s.verifyPage)},
		{"GET /unverified", http.HandlerFunc(s.unverifiedPage)},
		{"POST /resend", http.HandlerFunc(s.resendSubmit)},
		{"GET /forgot", http.HandlerFunc(s.forgotPage)},
		{"POST /forgot", http.HandlerFunc(s.forgotSubmit)},
		{"GET /reset", http.HandlerFunc(s.resetPage)},
		{"POST /reset", http.HandlerFunc(s.resetSubmit)},
		{"POST /logout", http.HandlerFunc(s.logout)},

		{"GET /apps", http.HandlerFunc(s.appsPage)},
		{"GET /apps/{slug}", http.HandlerFunc(s.appPage)},
		{"GET /apps/{slug}/releases", http.HandlerFunc(s.releasesPage)},
		{"GET /apps/{slug}/logs", http.HandlerFunc(s.logsPage)},
		{"GET /apps/{slug}/config", http.HandlerFunc(s.configPage)},
		{"POST /apps/{slug}/config/{key}/reveal", http.HandlerFunc(s.configReveal)},

		{"GET /connect", http.HandlerFunc(s.connectPage)},
		{"POST /connect", http.HandlerFunc(s.connectMint)},

		// The authorization server, spec 0024. The three machine endpoints are
		// namespaced under /oauth/ because POST /register above is already the
		// account signup form, and all three are advertised in the metadata
		// document so no client hardcodes a path (AC-25). Each names its method,
		// so a wrong verb on a path that exists is a 405 from the mux and a path
		// that does not exist is the host's own 404 (AC-25b).
		{"GET " + identity.AuthorizationServerPath, http.HandlerFunc(s.authServerDocument)},
		{"POST " + identity.RegisterPath, http.HandlerFunc(s.registerClient)},
		{"GET " + identity.AuthorizePath, http.HandlerFunc(s.authorizePage)},
		{"POST " + identity.AuthorizePath, http.HandlerFunc(s.authorizeSubmit)},
		{"POST " + identity.TokenPath, http.HandlerFunc(s.tokenExchange)},

		{"GET /tokens", http.HandlerFunc(s.tokensPage)},
		{"POST /tokens", http.HandlerFunc(s.tokenMint)},
		{"POST /tokens/{id}/revoke", http.HandlerFunc(s.tokenRevoke)},
	}
	for _, route := range public {
		mux.Handle(route.pattern, route.handler)
		if s.opts.ConsoleHost != "" {
			mux.Handle(withHost(route.pattern, s.opts.ConsoleHost), route.handler)
		}
	}

	// The admin surface, registered once. Absent from the console host, so it is
	// unreachable from the open internet by routing rather than by a check that
	// could be forgotten (AC-2).
	mux.HandleFunc("GET /admin/accounts", s.adminAccountsPage)
	mux.HandleFunc("POST /admin/accounts/{id}/disable", s.adminDisable)
	mux.HandleFunc("POST /admin/accounts/{id}/enable", s.adminEnable)
	mux.HandleFunc("POST /admin/accounts/{id}/tokens/{tokenId}/revoke", s.adminRevokeToken)

	mux.HandleFunc("GET /admin/backups", s.adminBackupsPage)
	mux.HandleFunc("POST /admin/backups/run", s.adminBackupRun)

	mux.HandleFunc("GET /admin/invites", s.adminInvitesPage)
	mux.HandleFunc("POST /admin/invites", s.adminInviteMint)
	mux.HandleFunc("POST /admin/invites/{id}/revoke", s.adminInviteRevoke)

	// The catch all, last. It carries no method, so it takes every verb, and it
	// is a subtree pattern, so every path the loop above did not claim on this
	// host lands here. A refusal writes no audit row: no caller has been
	// identified at this point, so there is nobody to record it against (AC-2).
	if s.opts.ConsoleHost != "" {
		mux.HandleFunc(s.opts.ConsoleHost+"/", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}
}

// withHost puts host in front of a mux pattern's path, keeping the method where
// the pattern has one. `GET /login` under `console.example.org` becomes
// `GET console.example.org/login`, which is the form the standard mux has taken
// since Go 1.22.
func withHost(pattern, host string) string {
	method, path, found := strings.Cut(pattern, " ")
	if !found {
		return host + pattern
	}
	return method + " " + host + path
}

// onConsole reports whether this request arrived on the public console hostname.
//
// It is the gate on reading CF-Connecting-IP and on showing the admin navigation,
// and it is deliberately a plain host comparison rather than anything cleverer:
// the routing split is enforced by the mux, and this only decides what a request
// that already reached a public page is allowed to be attributed to and shown.
// r.Host carries the port when one is in the request line, so it is stripped
// before comparing.
func (s *Server) onConsole(r *http.Request) bool {
	return s.opts.ConsoleHost != "" && hostOnly(r.Host) == s.opts.ConsoleHost
}

// hostOnly drops the port from a Host header value. A bare IPv6 literal keeps
// its brackets and its colons, which is why this looks for the last colon after
// the closing bracket rather than the first colon anywhere.
func hostOnly(host string) string {
	if i := strings.LastIndex(host, "]"); i >= 0 {
		host = host[:i+1]
		return host
	}
	if h, _, found := strings.Cut(host, ":"); found {
		return h
	}
	return host
}

// root sends a visitor to the one page that is useful to them: the app list with
// a live session, the sign in form without one (AC-2).
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.currentSession(r); ok {
		http.Redirect(w, r, "/apps", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// hostname is the address an app is served on, the slug joined with the
// wildcard domain the platform runs under.
func (s *Server) hostname(slug string) string {
	return slug + "." + s.opts.AppDomain
}

// safeNext is where a successful sign in lands. Only a local path is followed,
// and only one beginning with exactly one slash: two slashes is a protocol
// relative address, which a browser resolves to another host entirely (AC-2).
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/apps"
	}
	return next
}
