package domain

// Reason is why a deployment failed. The set is closed on purpose: it is what a
// caller sees, what `deployments.failure_reason` stores, and the whole of both.
// Build output, wrapped errors, and cluster messages never cross this boundary
// (spec 0004, AC-16).
type Reason string

// The nine failures a deploy can end in.
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
)

// messages is the one short sanitized line each code carries. Written for the
// agent that has to act on it: it says what to change, not what went wrong
// inside the platform.
var messages = map[Reason]string{
	ReasonUploadInvalid:   "the upload id is unknown, already used, or belongs to another account",
	ReasonUploadExpired:   "the upload expired, so upload the source again and retry",
	ReasonSourceRejected:  "the source archive was refused: it is too large, holds too many files, or contains an unsafe entry",
	ReasonBuildFailed:     "the build did not complete, so check that the app builds with Cloud Native Buildpacks",
	ReasonBuildNoDigest:   "the build reported success but pushed no image",
	ReasonImageRunsAsRoot: "the built image runs as root, so add a non root USER to the app",
	ReasonAppNeverReady:   "the app never accepted a connection on the port given in PORT",
	ReasonTimeout:         "the deploy ran out of time",
	ReasonInternal:        "the platform failed to complete the deploy",
}

// Valid reports whether r is one of the nine codes.
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
