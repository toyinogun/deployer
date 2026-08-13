package reconcile

import (
	"context"
	"log/slog"
	"time"
)

// ReapOrphanNamespaces deletes every app namespace whose slug has no live app
// row and that is older than the grace.
//
// It is the platform's first unattended destructive loop, so every guard here is
// load bearing: the label selector in internal/kube decides which namespaces are
// even considered, the live slug read decides which of those own nothing, the
// grace keeps a namespace a deploy created seconds ago out of reach, and a
// failed read aborts the whole pass rather than reading as "no app owns this"
// (spec 0012, AC-23, AC-24, AC-25, AC-26).
//
// An app's row exists from CreateApp, long before any namespace is composed for
// it, and stays live for the whole of every build, so an app mid deploy always
// has a live slug and is never a candidate whatever the grace is set to.
func (r *Reconciler) ReapOrphanNamespaces(ctx context.Context, now time.Time) {
	slugs, err := r.apps.LiveAppSlugs(ctx)
	if err != nil {
		// A database that cannot answer must never be read as "no app owns this",
		// so the pass reaps nothing at all (AC-24).
		slog.ErrorContext(ctx, "reading live app slugs to reap orphan namespaces failed, so nothing was reaped", "error", err)
		return
	}
	live := make(map[string]struct{}, len(slugs))
	for _, slug := range slugs {
		live[slug] = struct{}{}
	}

	candidates, err := r.cluster.AppNamespacesOlderThan(ctx, now, r.opts.OrphanGrace)
	if err != nil {
		slog.ErrorContext(ctx, "listing app namespaces to reap failed", "error", err)
		return
	}
	for _, slug := range candidates {
		if ctx.Err() != nil {
			return
		}
		if _, owned := live[slug]; owned {
			continue
		}
		// One namespace that will not delete is logged and stepped over, so the
		// rest of the pass still runs (AC-27).
		if err := r.cluster.DeleteNamespace(ctx, slug); err != nil {
			slog.ErrorContext(ctx, "reaping an orphan app namespace failed", "app", slug, "error", err)
			continue
		}
		slog.InfoContext(ctx, "orphan app namespace reaped", "app", slug)
	}
}
