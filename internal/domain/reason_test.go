package domain_test

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
)

// theSet is the whole closed set. Written out rather than derived, so adding a
// code without deciding what a caller is told fails here.
var theSet = []domain.Reason{
	domain.ReasonUploadInvalid,
	domain.ReasonUploadExpired,
	domain.ReasonSourceRejected,
	domain.ReasonBuildFailed,
	domain.ReasonBuildNoDigest,
	domain.ReasonImageRunsAsRoot,
	domain.ReasonAppNeverReady,
	domain.ReasonTimeout,
	domain.ReasonInternal,
	domain.ReasonDeploymentUnknown,
	domain.ReasonSuperseded,
	domain.ReasonAppUnknown,
	domain.ReasonReleaseUnknown,
	domain.ReasonConfigKeyInvalid,
	domain.ReasonConfigKeyReserved,
	domain.ReasonConfigKeyUnknown,
	domain.ReasonConfigFlagMissing,
	domain.ReasonConfigTooManyKeys,
	domain.ReasonConfigTooLarge,
	domain.ReasonDeploymentInFlight,
	domain.ReasonAppLimitReached,
	domain.ReasonAccountSuspended,
	domain.ReasonAppNameReserved,
	domain.ReasonUploadTooLarge,
	domain.ReasonUploadNotGzip,
	domain.ReasonUploadLimitReached,
	domain.ReasonTooManyAttempts,
}

// The set is closed at twenty seven codes: nine failures, plus
// deployment_unknown, superseded (spec 0005, AC-11), app_unknown
// (spec 0006, AC-8), the six configuration refusals (spec 0010), release_unknown
// (spec 0011, AC-21), deployment_in_flight (spec 0012, AC-15), app_limit_reached
// (spec 0016, AC-8), account_suspended (spec 0018), app_name_reserved,
// upload_too_large, upload_not_gzip, and the two spec 0022 added for the deploy
// path, upload_limit_reached and too_many_attempts (spec 0022, AC-19).
//
// This count is a reminder, not a guard: theSet is written by hand and nothing
// in the package reports how many codes it really holds, so a code added to
// reason.go and forgotten here is not caught by anything. It was forgotten six
// times between spec 0016 and spec 0022, which is how those six codes went
// untested for the properties below.
const codesInTheSet = 27

func TestTheSetIsExactlyTwentySevenCodes(t *testing.T) {
	// covers: AC-11
	t.Parallel()
	if len(theSet) != codesInTheSet {
		t.Fatalf("the set holds %d codes, want %d", len(theSet), codesInTheSet)
	}
}

func TestEveryReasonInTheSetIsValid(t *testing.T) {
	// covers: AC-16
	t.Parallel()
	for _, r := range theSet {
		if !r.Valid() {
			t.Errorf("%q is in the set but reads as invalid", r)
		}
	}
}

func TestAReasonOutsideTheSetIsRefused(t *testing.T) {
	// covers: AC-16
	t.Parallel()
	for _, r := range []domain.Reason{"", "unknown", "BUILD_FAILED", "build failed", "internal "} {
		if r.Valid() {
			t.Errorf("%q reads as valid but is not one of the closed set", r)
		}
	}
}

func TestEveryReasonCarriesOneShortSanitizedLine(t *testing.T) {
	// covers: AC-16
	t.Parallel()
	for _, r := range theSet {
		msg := r.Message()
		if msg == "" {
			t.Errorf("%q carries no message", r)
			continue
		}
		if strings.Contains(msg, "\n") {
			t.Errorf("%q spans more than one line: %q", r, msg)
		}
		if len(msg) > 160 {
			t.Errorf("%q is %d characters, which is long enough to be carrying detail: %q", r, len(msg), msg)
		}
	}
}

func TestNoReasonMessageLeaksWhatHappenedInsideThePlatform(t *testing.T) {
	// covers: AC-16
	t.Parallel()
	// The message is what an agent sees. Anything from a wrapped error, a build
	// log, or a cluster response has crossed a boundary it should not have.
	leaks := []string{
		"goroutine", ".go:", "sha256:", "kubectl", "http://", "https://",
		"deployer-system", "deployer-builds", "registry.", "client-go",
		"Error:", "panic", "sqlite", "namespace", "0x",
	}
	for _, r := range theSet {
		msg := r.Message()
		for _, leak := range leaks {
			if strings.Contains(msg, leak) {
				t.Errorf("%q carries %q, which is platform internals: %q", r, leak, msg)
			}
		}
	}
}

func TestEveryReasonSaysADifferentThing(t *testing.T) {
	// covers: AC-16
	t.Parallel()
	seen := map[string]domain.Reason{}

	for _, r := range theSet {
		if first, ok := seen[r.Message()]; ok {
			t.Errorf("%q and %q say the same thing, so the code carries no extra information", first, r)
		}
		seen[r.Message()] = r
	}
}

func TestAnUnknownReasonReadsAsAPlatformFault(t *testing.T) {
	// covers: AC-16
	t.Parallel()
	// A code that escaped the closed set is a platform bug, not news for the
	// agent, so the caller is told the platform failed rather than nothing.
	for _, r := range []domain.Reason{"", "unknown", "some_new_code"} {
		if got := r.Message(); got != domain.ReasonInternal.Message() {
			t.Errorf("%q reads as %q, want the internal message", r, got)
		}
	}
}

func TestBuildFailedNamesNoSingleEngine(t *testing.T) {
	// covers: spec 0009 AC-13
	t.Parallel()
	// Two engines can now produce this code, so the one message both of them
	// reach cannot tell the agent to go and fix the wrong thing. It points at
	// build_path instead, which is where the answer actually is. Naming an
	// engine here was correct while there was only one, which is exactly why
	// nothing would have caught it going stale.
	msg := domain.ReasonBuildFailed.Message()
	for _, engine := range []string{"buildpack", "cloud native", "paketo", "buildkit", "dockerfile"} {
		if strings.Contains(strings.ToLower(msg), engine) {
			t.Errorf("build_failed names %q, but either engine can produce it: %q", engine, msg)
		}
	}
	if !strings.Contains(msg, "build_path") {
		t.Errorf("build_failed does not send the agent to build_path, so it cannot tell which engine ran: %q", msg)
	}
}

func TestAReasonIsTheStringStoredOnTheDeploymentRow(t *testing.T) {
	// covers: AC-16
	t.Parallel()
	// The wire value, the database value, and the constant are the same string.
	// Renaming one silently breaks the other two.
	want := map[domain.Reason]string{
		domain.ReasonUploadInvalid:   "upload_invalid",
		domain.ReasonUploadExpired:   "upload_expired",
		domain.ReasonSourceRejected:  "source_rejected",
		domain.ReasonBuildFailed:     "build_failed",
		domain.ReasonBuildNoDigest:   "build_no_digest",
		domain.ReasonImageRunsAsRoot: "image_runs_as_root",
		domain.ReasonAppNeverReady:   "app_never_ready",
		domain.ReasonTimeout:         "timeout",
		domain.ReasonInternal:        "internal",

		domain.ReasonDeploymentUnknown: "deployment_unknown",
		domain.ReasonSuperseded:        "superseded",
		domain.ReasonAppUnknown:        "app_unknown",
		domain.ReasonReleaseUnknown:    "release_unknown",

		domain.ReasonConfigKeyInvalid:  "config_key_invalid",
		domain.ReasonConfigKeyReserved: "config_key_reserved",
		domain.ReasonConfigKeyUnknown:  "config_key_unknown",
		domain.ReasonConfigFlagMissing: "config_flag_missing",
		domain.ReasonConfigTooManyKeys: "config_too_many_keys",
		domain.ReasonConfigTooLarge:    "config_too_large",

		domain.ReasonDeploymentInFlight: "deployment_in_flight",
		domain.ReasonAppLimitReached:    "app_limit_reached",

		domain.ReasonAccountSuspended: "account_suspended",
		domain.ReasonAppNameReserved:  "app_name_reserved",

		// The upload path's own refusals. The last two are spec 0022, AC-19.
		domain.ReasonUploadTooLarge:     "upload_too_large",
		domain.ReasonUploadNotGzip:      "upload_not_gzip",
		domain.ReasonUploadLimitReached: "upload_limit_reached",
		domain.ReasonTooManyAttempts:    "too_many_attempts",
	}
	if len(want) != len(theSet) {
		t.Fatalf("the pinned map holds %d codes and the set holds %d", len(want), len(theSet))
	}
	for r, s := range want {
		if string(r) != s {
			t.Errorf("%q serialises as %q, want %q", r, string(r), s)
		}
	}
}
