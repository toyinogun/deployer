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
	// PublicURL is the platform's own reachable base address. It is the origin a
	// POST is compared against, and the base of the endpoints the onboarding
	// panel shows.
	PublicURL string
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
}

// Server is the page surface.
type Server struct {
	svc     *identity.Service
	auth    *auth.Authenticator
	auditor auth.Auditor
	data    Data
	pods    Pods
	opts    Options

	// origin is PublicURL reduced to scheme and host, precomputed because every
	// POST compares against it.
	origin string
	// secure is the cookie's Secure flag, derived from PublicURL exactly as the
	// JSON surface derives it, so the two cannot hand out different cookies.
	secure bool
}

// New returns the page surface. data is the store, pods may be nil when there is
// no cluster credential, which the logs page renders as an empty state rather
// than refusing to start.
func New(svc *identity.Service, a *auth.Authenticator, auditor auth.Auditor, data Data, pods Pods, opts Options) *Server {
	s := &Server{svc: svc, auth: a, auditor: auditor, data: data, pods: pods, opts: opts}
	if u, err := url.Parse(opts.PublicURL); err == nil && u.Host != "" {
		s.origin = u.Scheme + "://" + u.Host
		s.secure = u.Scheme == "https"
	}
	return s
}

// Register adds every page route to mux. The paths are the root ones: /v1 and
// /mcp are untouched by this package.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.root)
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("GET /register", s.registerPage)
	mux.HandleFunc("POST /register", s.registerSubmit)
	mux.HandleFunc("GET /verify", s.verifyPage)
	mux.HandleFunc("GET /unverified", s.unverifiedPage)
	mux.HandleFunc("POST /resend", s.resendSubmit)
	mux.HandleFunc("GET /forgot", s.forgotPage)
	mux.HandleFunc("POST /forgot", s.forgotSubmit)
	mux.HandleFunc("GET /reset", s.resetPage)
	mux.HandleFunc("POST /reset", s.resetSubmit)
	mux.HandleFunc("POST /logout", s.logout)

	mux.HandleFunc("GET /apps", s.appsPage)
	mux.HandleFunc("GET /apps/{slug}", s.appPage)
	mux.HandleFunc("GET /apps/{slug}/releases", s.releasesPage)
	mux.HandleFunc("GET /apps/{slug}/logs", s.logsPage)
	mux.HandleFunc("GET /apps/{slug}/config", s.configPage)
	mux.HandleFunc("POST /apps/{slug}/config/{key}/reveal", s.configReveal)

	mux.HandleFunc("GET /tokens", s.tokensPage)
	mux.HandleFunc("POST /tokens", s.tokenMint)
	mux.HandleFunc("POST /tokens/{id}/revoke", s.tokenRevoke)

	mux.HandleFunc("GET /admin/accounts", s.adminAccountsPage)
	mux.HandleFunc("POST /admin/accounts/{id}/disable", s.adminDisable)
	mux.HandleFunc("POST /admin/accounts/{id}/enable", s.adminEnable)
	mux.HandleFunc("POST /admin/accounts/{id}/tokens/{tokenId}/revoke", s.adminRevokeToken)
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
