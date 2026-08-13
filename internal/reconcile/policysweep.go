package reconcile

import (
	"context"
	"log/slog"

	"github.com/toyinogun/deployer/internal/deploy"
)

// PolicySweep applies the network fence to every app namespace on the cluster,
// whether or not anything is deploying to it.
//
// It reads the cluster rather than the database, so an app namespace created
// before this slice existed is policed without being redeployed, and it shares
// nothing with Sweep but the English word: no deployment state is read, written,
// or failed here (spec 0008, AC-12).
//
// A namespace that will not take its policies is logged and stepped over. One
// unreachable namespace must not leave the remaining ones unfenced, and there is
// no deployment in flight for this to fail.
func (r *Reconciler) PolicySweep(ctx context.Context) {
	slugs, err := r.cluster.AppNamespaces(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "listing app namespaces to police failed", "error", err)
		return
	}
	for _, slug := range slugs {
		if ctx.Err() != nil {
			return
		}
		// Only the fields the policies are composed from: this sweep knows a slug
		// and the configured blocked ranges, and needs nothing else.
		in := deploy.Input{Slug: slug, EgressBlockedCIDRs: r.opts.EgressBlockedCIDRs}
		if err := r.cluster.ApplyNetworkPolicies(ctx, deploy.DefaultDenyPolicy(in), deploy.AllowPolicy(in)); err != nil {
			slog.ErrorContext(ctx, "policing an app namespace failed", "app", slug, "error", err)
			continue
		}
		slog.InfoContext(ctx, "app namespace policed", "app", slug)
	}
}
