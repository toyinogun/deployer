# internal/web

The browser surface: server rendered pages over the same identity service and
store methods the JSON and MCP surfaces call. Read only for apps; every action
that changes an app stays with the agent over MCP. Designed in
[docs/specs/0013-web-interface/index.md](../../docs/specs/0013-web-interface/index.md).

## Layout

- `web.go`: the `Server`, the `Data` and `Pods` interfaces this package declares, and every route.
- `session.go`: the page session gate, the cookie, and the rate limit bucket.
- `csrf.go`: the derived synchroniser token and the origin check.
- `render.go`: the `go:embed` asset set, the template sets, the shell, and the template functions.
- `identity_pages.go`, `apps.go`, `tokens.go`, `admin.go`, `invites.go`: the handlers.
- `reason.go`: the plain sentence written for each closed reason code.
- `templates/`: `base.html` is the shell, `_partials.html` is the shared furniture, every other file is one page.
- `static/`: one stylesheet and one script, both embedded.

## Rules

- A page never reaches the database or Kubernetes except through a service or store method an existing surface already calls. A method on the `Data` interface that no other surface calls is the signal the layering broke.
- `store.ListConfigForDeploy` keeps exactly two callers, the deploy path and the release snapshot. The browser is not a third one: the logs page assembles its redaction literals from `ListConfigForResponse` for the secret flags and `CurrentReleaseConfig` for the values.
- The browser is never a weaker door than the agent surface. Anything MCP refuses to return, a page refuses to render, and a control that is merely absent from a template is not a guard: the route refuses it too.
- A page that takes a secret in its query string never validates it on `GET`. `/register` copies the invite code into a hidden field and renders identically for a live, spent, revoked, expired or made up one, so the page cannot be asked which kind a code is, and it answers `Referrer-Policy: no-referrer` so the code does not travel out in a referrer header.
- The CSRF token is the HMAC SHA256 of the session id under `DEPLOYER_CSRF_KEY`, derived at render and never stored, so it is valid exactly as long as its session and is revoked by the same act.
- Templates are parsed once at startup. A template names one page and defines `title` and `content`; two `content` blocks in one set is a redefinition error, which is why the shell branches around a single block rather than holding one per branch.
- The status fragment carries its own live marker inside the swapped content. An attribute on the container the script replaces would never change, and polling would never stop.
- Serving and last deploy are two independent facts everywhere they appear. Serving comes from the app's own current release, never from the latest deployment's release: a failed deployment has no release row, so that chain answers not found exactly when the page matters most.
- Every colour in the stylesheet is a custom property even though the feature is light theme only, because dark mode later is a token swap only if nothing hardcodes a colour now.
- The script is an improvement on pages that already work without it. With JavaScript off every page renders and every form submits; only live polling, copy to clipboard, and the collapsing sidebar are lost.
- `auth.Account.Name` is the platform's internal account name, not the person's. The shell shows the email address; showing `Name` is how the sidebar once read out an account id.
