package web

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/toyinogun/deployer/internal/auth"
)

// The whole page surface is embedded in the binary: the templates, the one
// stylesheet, and the one script. Nothing is read from disk at runtime and no
// node toolchain enters the repo, so the image gains no layer beyond the binary
// it already carried (AC-1).
//
//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// pages is every page template, each already joined to the shell and the
// partials, parsed once at startup. Parsing at startup rather than per request
// means a broken template is a panic on the first boot rather than a 500 on the
// page nobody opened until Friday.
var pages = parsePages()

// funcs is what a template may call. Deliberately small: formatting only, no
// lookup and no computation, because a template that can query is a page that
// holds a rule.
var funcs = template.FuncMap{
	"shortDigest": shortDigest,
	"when":        whenText,
	"tabs":        appTabs,
}

// tabData is what the app tab strip renders from. It is built by a function
// rather than assembled in the template, so the set of tabs lives in one place
// and a page cannot mark the wrong one current by typing a different string.
type tabData struct {
	Slug    string
	Current string
}

// appTabs is the template's way of asking for the tab strip on one app page.
func appTabs(slug, current string) tabData {
	return tabData{Slug: slug, Current: current}
}

// parsePages builds one template set per page: the shell, every partial, and
// that page's own file.
func parsePages() map[string]*template.Template {
	shared, err := fs.Glob(templateFS, "templates/_*.html")
	if err != nil {
		panic("web: globbing shared templates: " + err.Error())
	}
	shared = append(shared, "templates/base.html")

	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		panic("web: globbing page templates: " + err.Error())
	}
	out := make(map[string]*template.Template, len(files))
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(f, "templates/"), ".html")
		if strings.HasPrefix(name, "_") || name == "base" {
			continue
		}
		set := template.New("base").Funcs(funcs)
		out[name] = template.Must(set.ParseFS(templateFS, append(append([]string{}, shared...), f)...))
	}
	return out
}

// staticHandler serves the embedded stylesheet and script.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("web: opening the embedded static tree: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}

// shell is what every page needs regardless of what it shows: who is signed in,
// whether they are an administrator, the token their forms must carry, and which
// navigation entry is the current one.
type shell struct {
	SignedIn bool
	// Email is who is signed in, as the shell shows them. The account's display
	// name is deliberately not here: auth.Account carries the platform's own
	// internal account name rather than the person's, and showing that is how
	// the sidebar ended up reading out an account id.
	Email   string
	IsAdmin bool
	CSRF    string
	Nav     string
}

// pageData is what reaches a template: the shell, and that page's own value.
type pageData struct {
	Shell shell
	Data  any
}

// messagePage is the shape every standalone sentence page renders through: the
// link invalid page, the refusals, and the confirmations. One template for all
// of them is what keeps four different link failures reading identically (AC-7).
type messagePage struct {
	Title   string
	Message string
	// Action and ActionLabel are an optional link out, used for the sign in
	// action on the verified page and the resend action on the invalid one.
	Action      string
	ActionLabel string
}

// render writes one page for a signed in account.
func (s *Server) render(w http.ResponseWriter, r *http.Request, account auth.Account, sess auth.Session,
	status int, name, nav string, data any,
) {
	s.write(w, r, status, name, pageData{
		Shell: shell{
			SignedIn: true,
			Email:    account.Email,
			IsAdmin:  account.IsAdmin,
			CSRF:     s.csrfToken(sess.ID),
			Nav:      nav,
		},
		Data: data,
	})
}

// renderPublic writes a page with no session behind it: the identity forms and
// every standalone message.
func (s *Server) renderPublic(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	s.write(w, r, status, name, pageData{Data: data})
}

// renderRefused writes a refusal inside the signed in shell, so a person who is
// signed in and reached something that is not theirs keeps their navigation.
func (s *Server) renderRefused(w http.ResponseWriter, r *http.Request, account auth.Account, sess auth.Session,
	status int, title, message string,
) {
	s.render(w, r, account, sess, status, "message", "", messagePage{Title: title, Message: message})
}

// write renders to a buffer first, then copies. A template that fails halfway
// has already written a broken body if it renders straight to the response, and
// half a page with a 200 on it is worse than an error page.
func (s *Server) write(w http.ResponseWriter, r *http.Request, status int, name string, data pageData) {
	t, ok := pages[name]
	if !ok {
		slog.ErrorContext(r.Context(), "no such page template", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		slog.ErrorContext(r.Context(), "rendering a page failed", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A page is a session gated read of one account's data, so it must not be
	// held by a shared cache, and the back button must re fetch it rather than
	// show the previous account's list.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		slog.ErrorContext(r.Context(), "writing a page failed", "template", name, "error", err)
	}
}

// writeFragment renders one named partial on its own, for the status region the
// overview page polls. It carries no shell, because it replaces a region rather
// than a page.
func (s *Server) writeFragment(w http.ResponseWriter, r *http.Request, name, partial string, data any) {
	t, ok := pages[name]
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, partial, data); err != nil {
		slog.ErrorContext(r.Context(), "rendering a fragment failed", "partial", partial, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := buf.WriteTo(w); err != nil {
		slog.ErrorContext(r.Context(), "writing a fragment failed", "partial", partial, "error", err)
	}
}

// internalError is what a page answers when a read failed. The error is logged
// with its context and never shown: a wrapped error string is not a sentence a
// person can act on, and it is exactly where an internal detail leaks out.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error, what string) {
	slog.ErrorContext(r.Context(), what, "error", err)
	s.renderPublic(w, r, http.StatusInternalServerError, "message", messagePage{
		Title:   "Something went wrong",
		Message: "The platform failed while reading that. Try again in a moment.",
	})
}

// shortDigest is an image digest cut to something a person can compare at a
// glance. The full value stays in the title attribute the template sets.
func shortDigest(d string) string {
	if _, hex, found := strings.Cut(d, ":"); found && len(hex) > 12 {
		return hex[:12]
	}
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// whenText formats one of the store's timestamps for reading. They are RFC 3339
// strings on the row, and an unparseable one is shown as it stands rather than
// blanked, because a timestamp the platform wrote and cannot read back is worth
// seeing.
func whenText(raw string) string {
	if raw == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw
	}
	return t.UTC().Format("2 Jan 2006, 15:04 UTC")
}
