package domain

// Reason is why a deployment failed, or in the one case of ReasonSuperseded, why
// it was cancelled. The set is closed on purpose: it is what a caller sees, what
// `deployments.failure_reason` stores, and the whole of both. Build output,
// wrapped errors, and cluster messages never cross this boundary (spec 0004,
// AC-16).
type Reason string

// The closed set. Nine are failures a deploy can end on; ReasonSuperseded
// describes a cancellation, ReasonDeploymentUnknown, ReasonAppUnknown and
// ReasonReleaseUnknown a refused readback, ReasonDeploymentInFlight a refused
// delete, ReasonAppLimitReached a refused create, ReasonAccountSuspended every
// call an account makes while it is suspended, and the six config_ codes a
// refused configuration write.
const (
	ReasonUploadInvalid   Reason = "upload_invalid"
	ReasonUploadExpired   Reason = "upload_expired"
	ReasonSourceRejected  Reason = "source_rejected"
	ReasonBuildFailed     Reason = "build_failed"
	ReasonBuildNoDigest   Reason = "build_no_digest"
	ReasonImageRunsAsRoot Reason = "image_runs_as_root"
	ReasonAppNeverReady   Reason = "app_never_ready"
	ReasonTimeout         Reason = "timeout"
	ReasonInternal        Reason = "internal"
	// ReasonDeploymentUnknown is the one answer a status read gives for an id or
	// name that does not exist and for one belonging to another account.
	ReasonDeploymentUnknown Reason = "deployment_unknown"
	// ReasonAppUnknown is the one answer a log read gives for a name that does
	// not exist and for an app belonging to another account. It is separate from
	// ReasonDeploymentUnknown because get_logs is addressed by app name, and
	// answering about deployments and ids is how a closed reason set stops being
	// useful (spec 0006, Decision).
	ReasonAppUnknown Reason = "app_unknown"
	// ReasonSuperseded is why a deployment was cancelled: a later deploy of the
	// same app replaced it. A cancellation, not a failure.
	ReasonSuperseded Reason = "superseded"
	// ReasonReleaseUnknown is the one answer a rollback gives for a release
	// number the app does not have and for one that is not a positive integer.
	// Ownership of the app is decided first, so this code is only ever reached
	// on an app the caller owns (spec 0011, AC-7).
	ReasonReleaseUnknown Reason = "release_unknown"
	// ReasonDeploymentInFlight is why a delete_app is refused: the app has a
	// deploy or rollback still running, so tearing it down now would pull the
	// namespace out from under a build that is writing into it. Nothing is
	// written and nothing is torn down (spec 0012, AC-15).
	ReasonDeploymentInFlight Reason = "deployment_in_flight"
	// ReasonAppLimitReached is why a deploy of a name the account does not
	// already hold is refused: the account is at its ceiling for live apps.
	// Nothing is written, and everything the account already runs keeps
	// deploying. The message names no number, because the count and the cap
	// travel as a composed detail beside it (spec 0016, AC-8).
	ReasonAppLimitReached Reason = "app_limit_reached"
	// ReasonAccountSuspended is why every tool call and upload an account makes
	// is refused, and why a deployment already in flight when the suspension
	// lands ends failed. An admin suspended the account, so nothing it asks for
	// runs and nothing it runs keeps serving. Only an admin restoring it clears
	// this (spec 0018, AC-15).
	ReasonAccountSuspended Reason = "account_suspended"
	// ReasonAppNameReserved is why a create is refused: the name derives to a
	// hostname label the platform keeps for itself, the console's among them.
	// Nothing is written. It is decided on the create path only, so an app that
	// already holds one of these names keeps deploying (spec 0021, AC-6, AC-7).
	ReasonAppNameReserved Reason = "app_name_reserved"

	// The deploy path refusals, added by spec 0022 when that path went public.
	// Every one of them is decided before anything is stored, so a refused call
	// leaves the volume and the database untouched (AC-12, AC-17, AC-19).

	// ReasonUploadTooLarge is a body over the upload ceiling. A declared length
	// past it is refused before a byte is read, and a body that declared nothing
	// or lied is stopped at the socket. The ceiling is strictly under the edge's
	// own cap, so this is always the platform's answer rather than an error page
	// from Cloudflare (AC-11, AC-12).
	ReasonUploadTooLarge Reason = "upload_too_large"
	// ReasonUploadNotGzip is a body that did not start as a gzip stream. Nothing
	// is kept.
	ReasonUploadNotGzip Reason = "upload_not_gzip"
	// ReasonUploadLimitReached is why an upload is refused: the account already
	// holds its ceiling of uploads that no deploy has claimed. Nothing is written
	// to the volume, and deploying or expiring one of the held uploads frees a
	// slot (AC-17).
	ReasonUploadLimitReached Reason = "upload_limit_reached"
	// ReasonTooManyAttempts is why a call on the deploy path is refused 429: the
	// address is calling faster than the bucket refills, or it is inside the
	// penalty window a run of bad tokens earned. It is deliberately separate from
	// identity.CodeRateLimited, because the two closed sets share no values and a
	// machine surface refusal needs its own name (AC-15, AC-16, AC-19).
	ReasonTooManyAttempts Reason = "too_many_attempts"

	// The configuration refusals, added by spec 0010. Every one of them is
	// decided before any write happens, so a refused call changes nothing.

	// ReasonConfigKeyInvalid is a key that is not an environment variable name,
	// an empty call, or one naming the same key twice.
	ReasonConfigKeyInvalid Reason = "config_key_invalid"
	// ReasonConfigKeyReserved is PORT or APP_URL, which the platform injects
	// itself and a caller may never store.
	ReasonConfigKeyReserved Reason = "config_key_reserved"
	// ReasonConfigKeyUnknown is an unset_config naming a key the app does not
	// have. The whole call is refused and nothing is removed.
	ReasonConfigKeyUnknown Reason = "config_key_unknown"
	// ReasonConfigFlagMissing is a key sent without its secret flag, which is
	// required on every write so a secret can never quietly become plain.
	ReasonConfigFlagMissing Reason = "config_flag_missing"
	// ReasonConfigTooManyKeys is a call that would take the app past
	// MaxConfigKeys.
	ReasonConfigTooManyKeys Reason = "config_too_many_keys"
	// ReasonConfigTooLarge is a single value past MaxConfigValueBytes, or a
	// whole configuration past MaxConfigTotalBytes.
	ReasonConfigTooLarge Reason = "config_too_large"
)

// messages is the one short sanitized line each code carries. Written for the
// agent that has to act on it: it says what to change, not what went wrong
// inside the platform.
var messages = map[Reason]string{
	ReasonUploadInvalid:   "the upload id is unknown, already used, or belongs to another account",
	ReasonUploadExpired:   "the upload expired, so upload the source again and retry",
	ReasonSourceRejected:  "the source archive was refused: it is too large, holds too many files, or contains an unsafe entry",
	ReasonBuildFailed:     "the build did not complete, so check that the app builds with the engine that ran, which deployment_status reports as build_path",
	ReasonBuildNoDigest:   "the build reported success but pushed no image",
	ReasonImageRunsAsRoot: "the built image runs as root, so add a non root USER to the app",
	ReasonAppNeverReady:   "the app never accepted a connection on the port given in PORT",
	ReasonTimeout:         "the deploy ran out of time",
	ReasonInternal:        "the platform failed to complete the deploy",

	ReasonDeploymentUnknown: "no deployment matches that id or name",
	ReasonAppUnknown:        "no app matches that name",
	ReasonSuperseded:        "a later deploy of the same app replaced this one",
	ReasonReleaseUnknown:    "that app has no release with that number, so call list_releases to see which numbers exist",

	ReasonDeploymentInFlight: "that app has a deploy or rollback still running, so wait for it to finish and try again",

	ReasonAppLimitReached: "this account is at its limit for apps, so delete one you no longer need",

	ReasonAccountSuspended: "this account is suspended, so its apps are stopped and nothing it asks for will run until an administrator restores it",

	ReasonAppNameReserved: "that name is reserved by the platform, so pick another one",

	ReasonUploadTooLarge:     "the source archive is larger than this platform accepts, so leave out build output, dependencies and version control directories",
	ReasonUploadNotGzip:      "the body must be a gzipped tar archive",
	ReasonUploadLimitReached: "this account is holding as many unused uploads as it may, so deploy one of them or wait for them to expire",
	ReasonTooManyAttempts:    "too many calls from this address, so wait a little and try again",

	ReasonConfigKeyInvalid:  "a configuration key must be upper case letters, digits, and underscores, not start with a digit, and appear once per call",
	ReasonConfigKeyReserved: "PORT and APP_URL are set by the platform and cannot be configured",
	ReasonConfigKeyUnknown:  "one of those keys is not set on this app, so nothing was removed",
	ReasonConfigFlagMissing: "every key needs its secret flag, so send secret true or false with each one",
	ReasonConfigTooManyKeys: "that would take the app past 64 configuration keys",
	ReasonConfigTooLarge:    "a value may be at most 4 KB and an app's whole configuration at most 32 KB",
}

// Valid reports whether r is one of the closed set.
func (r Reason) Valid() bool {
	_, ok := messages[r]
	return ok
}

// Message is the one line a caller is told. An unknown code reads as internal,
// because a code that escaped the set is a platform bug, not news for the agent.
func (r Reason) Message() string {
	if m, ok := messages[r]; ok {
		return m
	}
	return messages[ReasonInternal]
}
