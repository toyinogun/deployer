package web

import "github.com/toyinogun/deployer/internal/domain"

// sentences is the plain line shown for each closed reason code.
//
// It lives here rather than in internal/domain because these are written for a
// person reading a page, and the domain's own messages are written for an agent
// reading a tool response. The cost is that this table can drift from
// reason.go; the fallback below is what keeps drift from breaking the page
// (AC-17).
var sentences = map[domain.Reason]string{
	domain.ReasonUploadInvalid:   "The uploaded source could not be read.",
	domain.ReasonUploadExpired:   "The uploaded source expired before the build started.",
	domain.ReasonSourceRejected:  "The source was refused before building. Check it meets the rules in the deploy tool's description.",
	domain.ReasonBuildFailed:     "The build failed. Its output is in the build pod's logs, not here.",
	domain.ReasonBuildNoDigest:   "The build finished but produced no image digest.",
	domain.ReasonImageRunsAsRoot: "The built image runs as root, which is not allowed.",
	domain.ReasonAppNeverReady:   "The app started but never became ready. Check its logs.",
	domain.ReasonTimeout:         "The deploy ran out of time.",
	domain.ReasonInternal:        "The platform failed while deploying.",
	domain.ReasonSuperseded:      "Cancelled because a newer deploy replaced it.",
	// Unlike the other refusal codes, this one can reach deployments.failure_reason:
	// a deploy already in flight when its account is suspended ends here (spec
	// 0018, AC-14).
	domain.ReasonAccountSuspended: "Stopped because this account was suspended.",
}

// reasonSentence is the line to show for a reason code, falling back to the code
// itself. A code added to internal/domain later and not written up here degrades
// to showing the raw code rather than rendering an empty element (AC-17).
func reasonSentence(r domain.Reason) string {
	if s, ok := sentences[r]; ok {
		return s
	}
	return string(r)
}
