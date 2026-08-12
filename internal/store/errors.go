package store

import (
	"errors"
	"fmt"
)

// The errors callers branch on. Anything else is wrapped with fmt.Errorf and
// treated as a fault rather than a decision.
var (
	// ErrNotFound is returned when a row does not exist, or exists but is soft
	// deleted, which reads the same from outside.
	ErrNotFound = errors.New("store: not found")

	// ErrTokenInvalid covers unknown, revoked, and expired tokens, and tokens
	// belonging to a disabled account. They are deliberately indistinguishable.
	ErrTokenInvalid = errors.New("store: token invalid")

	// ErrSlugTaken means slug generation could not find a free slug within its
	// retry budget.
	ErrSlugTaken = errors.New("store: slug taken")

	// ErrAppNameTaken means the account already has a live app with that name.
	ErrAppNameTaken = errors.New("store: app name taken")

	// ErrAppDeleted means the app exists but has been soft deleted.
	ErrAppDeleted = errors.New("store: app deleted")

	// ErrDeploymentInFlight blocks a soft delete while a deployment is running.
	ErrDeploymentInFlight = errors.New("store: deployment in flight")

	// ErrDeploymentSourceAmbiguous means a deployment named both an upload and a
	// source release, or neither.
	ErrDeploymentSourceAmbiguous = errors.New("store: deployment must name exactly one of upload or source release")

	// ErrIllegalTransition means the requested move is not in the state machine.
	ErrIllegalTransition = errors.New("store: illegal state transition")

	// ErrTerminal means the move was refused because the deployment had already
	// reached a terminal state, which is what a supersession does to a row while
	// the loop is still driving it. It is a kind of ErrIllegalTransition, so a
	// caller that only cares that the move was refused still matches, and one
	// that has to tell "something else ended this row" from "this move is not in
	// the machine" can branch on it.
	ErrTerminal = fmt.Errorf("store: the deployment is already terminal: %w", ErrIllegalTransition)

	// ErrNoDigest means a deployment reached the point of becoming a release
	// without an image to record.
	ErrNoDigest = errors.New("store: deployment has no image digest")

	// ErrReleaseExists means a release already exists for this deployment.
	ErrReleaseExists = errors.New("store: release already exists for deployment")

	// ErrUploadExpired means the upload's one hour window has passed.
	ErrUploadExpired = errors.New("store: upload expired")

	// ErrUploadRedeemed means the single use fetch token was already spent.
	ErrUploadRedeemed = errors.New("store: upload already redeemed")

	// ErrInvalidKey means a configuration key does not match the platform's
	// naming rule.
	ErrInvalidKey = errors.New("store: invalid configuration key")
)
