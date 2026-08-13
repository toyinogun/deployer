package domain

// Reason is why a deployment failed, or in the one case of ReasonSuperseded, why
// it was cancelled. The set is closed on purpose: it is what a caller sees, what
// `deployments.failure_reason` stores, and the whole of both. Build output,
// wrapped errors, and cluster messages never cross this boundary (spec 0004,
// AC-16).
type Reason string

// The nineteen codes. Nine are failures a deploy can end on; ReasonSuperseded
// describes a cancellation, ReasonDeploymentUnknown, ReasonAppUnknown and
// ReasonReleaseUnknown a refused readback, and the six config_ codes a refused
// configuration write.
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

	ReasonConfigKeyInvalid:  "a configuration key must be upper case letters, digits, and underscores, not start with a digit, and appear once per call",
	ReasonConfigKeyReserved: "PORT and APP_URL are set by the platform and cannot be configured",
	ReasonConfigKeyUnknown:  "one of those keys is not set on this app, so nothing was removed",
	ReasonConfigFlagMissing: "every key needs its secret flag, so send secret true or false with each one",
	ReasonConfigTooManyKeys: "that would take the app past 64 configuration keys",
	ReasonConfigTooLarge:    "a value may be at most 4 KB and an app's whole configuration at most 32 KB",
}

// Valid reports whether r is one of the nineteen codes.
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
