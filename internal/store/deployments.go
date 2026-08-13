package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/ids"
	"github.com/toyinogun/deployer/internal/store/sqlcgen"
)

// The row types callers read. They are the generated models directly: adding a
// parallel set of structs would buy nothing but a mapping layer to keep in sync.
type (
	// Account is one caller identity.
	Account = sqlcgen.Account
	// APIToken is one bearer credential, stored only as a hash.
	APIToken = sqlcgen.ApiToken
	// App is one deployed application.
	App = sqlcgen.App
	// Upload is one source tarball waiting for a build.
	Upload = sqlcgen.Upload
	// Deployment is one attempt to get a version of an app running.
	Deployment = sqlcgen.Deployment
	// DeploymentEvent is one recorded state change.
	DeploymentEvent = sqlcgen.DeploymentEvent
	// Release is one known good image plus the configuration it ran with.
	Release = sqlcgen.Release
)

// CreateDeploymentInput names everything a caller supplies to start a deployment.
// Exactly one of UploadID and SourceReleaseID must be set: an upload makes it a
// build deploy, a source release makes it a rollback.
type CreateDeploymentInput struct {
	AppID           string
	AccountID       string
	UploadID        *string
	SourceReleaseID *string
}

// CreateDeployment starts a deployment for an app. If one is already in flight
// it is cancelled with reason "superseded" first, with its own event row, and
// the whole thing (cancel, cancel event, insert, first event) is one transaction.
// The returned supersededID is empty when nothing was in flight.
//
// On a rollback the image digest is copied from the source release at creation,
// because no build will ever run to fill it in.
func (s *Store) CreateDeployment(ctx context.Context, in CreateDeploymentInput) (Deployment, string, error) {
	if (in.UploadID == nil) == (in.SourceReleaseID == nil) {
		return Deployment{}, "", ErrDeploymentSourceAmbiguous
	}

	var out Deployment
	var supersededID string
	now := s.now()

	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		if _, err := q.GetApp(ctx, in.AppID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAppDeleted
			}
			return fmt.Errorf("store: reading app %s: %w", in.AppID, err)
		}

		var digest *string
		if in.SourceReleaseID != nil {
			rel, err := q.GetRelease(ctx, *in.SourceReleaseID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return fmt.Errorf("store: reading source release: %w", err)
			}
			if rel.AppID != in.AppID {
				return fmt.Errorf("store: release %s belongs to app %s, not %s: %w",
					rel.ID, rel.AppID, in.AppID, ErrNotFound)
			}
			digest = ptr(rel.ImageDigest)
		}

		// Supersede whatever is in flight before the partial unique index can
		// refuse the insert.
		inFlight, err := q.GetInFlightDeploymentForApp(ctx, in.AppID)
		switch {
		case err == nil:
			// The reason goes on the row as well as its event, so a status read of
			// the cancelled deployment needs no special case (spec 0005, AC-12).
			if _, err := q.UpdateDeploymentState(ctx, sqlcgen.UpdateDeploymentStateParams{
				State:         string(domain.StateCancelled),
				FailureReason: ptr(string(domain.ReasonSuperseded)),
				FinishedAt:    ptr(now),
				Now:           now,
				ID:            inFlight.ID,
			}); err != nil {
				return fmt.Errorf("store: superseding deployment %s: %w", inFlight.ID, err)
			}
			if err := q.InsertDeploymentEvent(ctx, sqlcgen.InsertDeploymentEventParams{
				ID:           ids.New(ids.DeploymentEvent, s.clock.Now()),
				DeploymentID: inFlight.ID,
				FromState:    ptr(inFlight.State),
				ToState:      string(domain.StateCancelled),
				Reason:       ptr(string(domain.ReasonSuperseded)),
				OccurredAt:   now,
			}); err != nil {
				return fmt.Errorf("store: recording supersession of %s: %w", inFlight.ID, err)
			}
			supersededID = inFlight.ID
		case errors.Is(err, sql.ErrNoRows):
			// Nothing in flight, which is the normal case.
		default:
			return fmt.Errorf("store: looking for an in flight deployment: %w", err)
		}

		id := ids.New(ids.Deployment, s.clock.Now())
		out, err = q.CreateDeployment(ctx, sqlcgen.CreateDeploymentParams{
			ID:              id,
			AppID:           in.AppID,
			AccountID:       in.AccountID,
			UploadID:        in.UploadID,
			SourceReleaseID: in.SourceReleaseID,
			ImageDigest:     digest,
			CreatedAt:       now,
			UpdatedAt:       now,
		})
		if err != nil {
			return fmt.Errorf("store: creating deployment: %w", err)
		}
		// The row's birth is an event too, with no from state.
		if err := q.InsertDeploymentEvent(ctx, sqlcgen.InsertDeploymentEventParams{
			ID:           ids.New(ids.DeploymentEvent, s.clock.Now()),
			DeploymentID: id,
			ToState:      string(domain.StateQueued),
			OccurredAt:   now,
		}); err != nil {
			return fmt.Errorf("store: recording deployment creation: %w", err)
		}
		return nil
	})
	if err != nil {
		return Deployment{}, "", err
	}
	return out, supersededID, nil
}

// Transition moves a deployment to a new state, writing the row and exactly one
// event in a single transaction. The from state is read inside that transaction,
// never supplied by the caller, and an illegal move writes nothing.
//
// started_at is stamped on the way out of queued and finished_at on the way into
// any terminal state, so the retention sweep has something to measure against.
func (s *Store) Transition(ctx context.Context, deploymentID string, to domain.State, reason, detail string) (Deployment, error) {
	if to == domain.StateHealthy {
		// Becoming healthy also mints a release, which MarkHealthy owns whole.
		return Deployment{}, fmt.Errorf("store: use MarkHealthy to reach healthy: %w", ErrIllegalTransition)
	}

	var out Deployment
	now := s.now()
	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		dep, err := q.GetDeployment(ctx, deploymentID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("store: reading deployment %s: %w", deploymentID, err)
		}
		from := domain.State(dep.State)
		if from.Terminal() {
			// Something else ended this row first, which is a race the loop has to
			// tell apart from a move that was never in the machine.
			return fmt.Errorf("store: %s to %s: %w", from, to, ErrTerminal)
		}
		if !domain.CanTransition(from, to) {
			return fmt.Errorf("store: %s to %s: %w", from, to, ErrIllegalTransition)
		}
		out, err = s.applyTransition(ctx, q, dep, to, reason, detail, now)
		return err
	})
	if err != nil {
		return Deployment{}, err
	}
	return out, nil
}

// applyTransition writes the row update and its event. It assumes the move has
// already been checked and that it is running inside a transaction.
func (s *Store) applyTransition(ctx context.Context, q *sqlcgen.Queries, dep Deployment, to domain.State, reason, detail, now string) (Deployment, error) {
	from := domain.State(dep.State)
	params := sqlcgen.UpdateDeploymentStateParams{
		State: string(to),
		Now:   now,
		ID:    dep.ID,
	}
	if from == domain.StateQueued {
		params.StartedAt = ptr(now)
	}
	if to.Terminal() {
		params.FinishedAt = ptr(now)
	}
	if to == domain.StateFailed && reason != "" {
		params.FailureReason = ptr(reason)
	}

	updated, err := q.UpdateDeploymentState(ctx, params)
	if err != nil {
		return Deployment{}, fmt.Errorf("store: moving deployment %s to %s: %w", dep.ID, to, err)
	}

	event := sqlcgen.InsertDeploymentEventParams{
		ID:           ids.New(ids.DeploymentEvent, s.clock.Now()),
		DeploymentID: dep.ID,
		FromState:    ptr(string(from)),
		ToState:      string(to),
		OccurredAt:   now,
	}
	if reason != "" {
		event.Reason = ptr(reason)
	}
	if detail != "" {
		event.Detail = ptr(detail)
	}
	if err := q.InsertDeploymentEvent(ctx, event); err != nil {
		return Deployment{}, fmt.Errorf("store: recording %s to %s on %s: %w", from, to, dep.ID, err)
	}
	return updated, nil
}

// BuildResult is what the reconcile loop reads back off a finished build Job.
type BuildResult struct {
	BuildPath    string // "buildpacks" or "dockerfile"
	BuildJobName string
	ImageRepo    string
	ImageDigest  string
}

// RecordBuildResult stores what the build produced. It does not move the
// deployment; the caller transitions separately, so a build that reported a
// digest but failed health checks still has its digest on the row.
func (s *Store) RecordBuildResult(ctx context.Context, deploymentID string, r BuildResult) error {
	params := sqlcgen.RecordBuildResultParams{Now: s.now(), ID: deploymentID}
	if r.BuildPath != "" {
		params.BuildPath = ptr(r.BuildPath)
	}
	if r.BuildJobName != "" {
		params.BuildJobName = ptr(r.BuildJobName)
	}
	if r.ImageRepo != "" {
		params.ImageRepo = ptr(r.ImageRepo)
	}
	if r.ImageDigest != "" {
		params.ImageDigest = ptr(r.ImageDigest)
	}
	n, err := s.q.RecordBuildResult(ctx, params)
	if err != nil {
		return fmt.Errorf("store: recording build result for %s: %w", deploymentID, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkHealthy is the one path that creates a release, and it does the whole thing
// in one transaction: the transition, its event, the release row carrying the
// digest read off the deployment and a snapshot of the app's configuration, and
// the app's current release pointer. There is deliberately no other way to make a
// release, because any other way reopens the window where a healthy deployment
// has none.
//
// The config is the caller's, not a fresh read: the deploy composed the
// container's Secret from one read minutes earlier, and re reading here would
// snapshot a set_config that landed during the readiness wait onto a release
// that never ran it (spec 0010, AC-10).
func (s *Store) MarkHealthy(ctx context.Context, deploymentID string, config map[string]string) (Deployment, Release, error) {
	var dep Deployment
	var rel Release
	now := s.now()

	err := s.inTx(ctx, func(q *sqlcgen.Queries) error {
		current, err := q.GetDeployment(ctx, deploymentID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("store: reading deployment %s: %w", deploymentID, err)
		}
		from := domain.State(current.State)
		if from.Terminal() {
			return fmt.Errorf("store: %s to healthy: %w", from, ErrTerminal)
		}
		if !domain.CanTransition(from, domain.StateHealthy) {
			return fmt.Errorf("store: %s to healthy: %w", from, ErrIllegalTransition)
		}
		digest := deref(current.ImageDigest)
		if digest == "" {
			return ErrNoDigest
		}

		dep, err = s.applyTransition(ctx, q, current, domain.StateHealthy, "", "", now)
		if err != nil {
			return err
		}

		snapshot, err := configSnapshot(config)
		if err != nil {
			return err
		}
		number, err := q.NextReleaseNumber(ctx, current.AppID)
		if err != nil {
			return fmt.Errorf("store: numbering the release for %s: %w", current.AppID, err)
		}

		rel, err = q.InsertRelease(ctx, sqlcgen.InsertReleaseParams{
			ID:             ids.New(ids.Release, s.clock.Now()),
			AppID:          current.AppID,
			DeploymentID:   current.ID,
			ReleaseNumber:  number,
			ImageDigest:    digest,
			ConfigSnapshot: snapshot,
			CreatedAt:      now,
		})
		if err != nil {
			if isUniqueViolation(err) {
				return ErrReleaseExists
			}
			return fmt.Errorf("store: creating the release for %s: %w", current.ID, err)
		}

		if _, err := q.SetAppCurrentRelease(ctx, sqlcgen.SetAppCurrentReleaseParams{
			ReleaseID: ptr(rel.ID),
			Now:       now,
			ID:        current.AppID,
		}); err != nil {
			return fmt.Errorf("store: pointing app %s at release %s: %w", current.AppID, rel.ID, err)
		}
		return nil
	})
	if err != nil {
		return Deployment{}, Release{}, err
	}
	return dep, rel, nil
}

// configSnapshot encodes the configuration a deploy composed with, secret values
// included, as the JSON a release stores. It takes the values rather than reading
// them, because the snapshot has to describe what the running pod was given, not
// what the table holds at the moment the release is cut.
func configSnapshot(values map[string]string) (string, error) {
	if values == nil {
		values = map[string]string{}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("store: encoding the configuration snapshot: %w", err)
	}
	return string(encoded), nil
}

// ClaimNext hands the oldest unclaimed queued deployment to one caller and only
// one. A racing caller updates no rows and gets ErrNotFound, which is a normal
// result for a reconcile loop with nothing to do.
func (s *Store) ClaimNext(ctx context.Context, claimedBy string) (Deployment, error) {
	dep, err := s.q.ClaimNextDeployment(ctx, sqlcgen.ClaimNextDeploymentParams{
		Now:       ptr(s.now()),
		ClaimedBy: ptr(claimedBy),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("store: claiming the next deployment: %w", err)
	}
	return dep, nil
}

// GetDeployment reads one deployment.
func (s *Store) GetDeployment(ctx context.Context, id string) (Deployment, error) {
	dep, err := s.q.GetDeployment(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("store: reading deployment %s: %w", id, err)
	}
	return dep, nil
}

// GetLatestDeploymentForApp reads an app's most recent deployment, which is what
// a status read by app name reports. ErrNotFound means the app has never been
// deployed.
func (s *Store) GetLatestDeploymentForApp(ctx context.Context, appID string) (Deployment, error) {
	dep, err := s.q.GetLatestDeploymentForApp(ctx, appID)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("store: reading the latest deployment for app %s: %w", appID, err)
	}
	return dep, nil
}

// GetNextDeploymentForApp reads the deployment that came after this one for the
// same app, which is what superseded_by is derived from. Ordering is by id, a
// monotonic ULID, never by created_at (spec 0005, AC-13). ErrNotFound means
// there is no later deployment yet.
func (s *Store) GetNextDeploymentForApp(ctx context.Context, appID, after string) (Deployment, error) {
	dep, err := s.q.GetNextDeploymentForApp(ctx, sqlcgen.GetNextDeploymentForAppParams{AppID: appID, After: after})
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("store: reading the deployment after %s: %w", after, err)
	}
	return dep, nil
}

// ListNonTerminalDeployments returns everything still in flight, which is what
// the reconcile loop sweeps on startup.
func (s *Store) ListNonTerminalDeployments(ctx context.Context) ([]Deployment, error) {
	deps, err := s.q.ListNonTerminalDeployments(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing in flight deployments: %w", err)
	}
	return deps, nil
}

// ListDeploymentsByApp returns one page of an app's deployments, newest first.
func (s *Store) ListDeploymentsByApp(ctx context.Context, appID string, page Page) ([]Deployment, error) {
	deps, err := s.q.ListDeploymentsByApp(ctx, sqlcgen.ListDeploymentsByAppParams{
		AppID:     appID,
		Cursor:    page.Cursor,
		PageLimit: page.limit(),
	})
	if err != nil {
		return nil, fmt.Errorf("store: listing deployments for app %s: %w", appID, err)
	}
	return deps, nil
}

// ListDeploymentEvents returns a deployment's whole event log in order.
func (s *Store) ListDeploymentEvents(ctx context.Context, deploymentID string) ([]DeploymentEvent, error) {
	events, err := s.q.ListDeploymentEvents(ctx, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("store: reading the event log for %s: %w", deploymentID, err)
	}
	return events, nil
}

// GetRelease reads one release.
func (s *Store) GetRelease(ctx context.Context, id string) (Release, error) {
	rel, err := s.q.GetRelease(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Release{}, ErrNotFound
	}
	if err != nil {
		return Release{}, fmt.Errorf("store: reading release %s: %w", id, err)
	}
	return rel, nil
}

// ListReleasesByApp returns one page of an app's releases, newest first.
func (s *Store) ListReleasesByApp(ctx context.Context, appID string, page Page) ([]Release, error) {
	rels, err := s.q.ListReleasesByApp(ctx, sqlcgen.ListReleasesByAppParams{
		AppID:     appID,
		Cursor:    page.Cursor,
		PageLimit: page.limit(),
	})
	if err != nil {
		return nil, fmt.Errorf("store: listing releases for app %s: %w", appID, err)
	}
	return rels, nil
}
