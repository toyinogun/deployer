// Package logs holds the pure part of reading an app's own output: parsing a
// kubelet timestamped line, redacting it, clamping a requested tail, and
// bounding a block of entries to a ceiling.
//
// It imports no client-go and no net/http on purpose. The cluster read lives in
// internal/kube and the tool that joins them in internal/mcp, so everything that
// decides what a caller actually receives is testable without a cluster
// (spec 0006, Build plan).
package logs

import (
	"errors"
	"regexp"
	"strings"
)

// The bounds of one log read. They are constants rather than configuration
// because they are product decisions about what fits an agent's context window,
// not per deployment tuning (spec 0006, Configuration required).
const (
	// DefaultTail is the line count a caller that asks for none gets.
	DefaultTail = 200
	// MaxTail is the most lines a caller may ask for. A larger ask is clamped to
	// this rather than refused (AC-2).
	MaxTail = 1000
	// CurrentBytes caps the current container's block regardless of the tail
	// asked for (AC-3).
	CurrentBytes = 64 * 1024
	// PreviousLines and PreviousBytes cap the previous container's block
	// independently, so restart noise in the current container can never squeeze
	// the crash out of the answer (AC-4).
	PreviousLines = 100
	PreviousBytes = 16 * 1024
	// BrowserTail and BrowserBytes cap what one page render shows. They are
	// separate from the agent's bounds and larger, because a scrollable pane is
	// not a context window, and they are constants for the same reason the pair
	// above are: what fits a reader is a product decision, not per deployment
	// tuning (spec 0013, AC-19).
	BrowserTail  = 500
	BrowserBytes = 256 * 1024
)

// redactedMark is what stands in for every blanked value, so a caller can see
// that something was removed rather than that nothing was there.
const redactedMark = "[REDACTED]"

// minLiteral is the shortest literal secret worth substring matching. Anything
// shorter appears in ordinary output constantly, and blanking it would destroy
// the lines it is meant to protect.
const minLiteral = 8

// Entry is one log record: the timestamp the kubelet recorded and the line.
type Entry struct {
	At      string `json:"at"`
	Message string `json:"message"`
}

// ErrNoNamespace reports that an app's namespace holds nothing the platform can
// see yet. An app's namespace and the RoleBinding that reaches into it are both
// created at the deploy step, which runs only once the build has finished, so a
// log read during the build is refused by Kubernetes rather than answered with
// an empty list. Refused and absent are the same answer here, because Kubernetes
// declines to say whether a namespace you hold no binding in exists at all, and
// both mean the app's container has not started (spec 0006, AC-7).
var ErrNoNamespace = errors.New("logs: the app's namespace is not readable yet")

// PodStatus is everything the tool needs to decide whether there is anything to
// fetch, before it calls the log API at all. It lives here rather than in
// internal/kube so the tool can reason about a pod without importing client-go
// (spec 0006, Key invariants).
type PodStatus struct {
	Name string
	// Ready is the pod's own Ready condition. A newest pod that is not ready
	// while an older one still exists is what the "an older pod may still be
	// serving" note is derived from (AC-5).
	Ready bool
	// ContainerStarted is false when the app container has no status yet or is
	// still Waiting, which is the empty case rather than a fault (AC-7).
	ContainerStarted bool
	// RestartCount above zero is the only thing that makes a previous container
	// block exist (AC-4).
	RestartCount int32
}

// ClampTail turns a requested tail into the one actually applied, and reports
// whether it differed from a real ask. Absent, zero, and negative all read as
// unset, which means the default rather than a clamp (AC-2).
func ClampTail(requested int) (applied int, clamped bool) {
	if requested <= 0 {
		return DefaultTail, false
	}
	if requested > MaxTail {
		return MaxTail, true
	}
	return requested, false
}

// Parse splits a kubelet response read with timestamps into entries, oldest
// first, which is the order the kubelet writes them in.
//
// A line the kubelet did not stamp keeps an empty timestamp rather than being
// dropped: it is still the app's output, and inventing a time for it would be
// worse than admitting there is none.
func Parse(raw string) []Entry {
	lines := strings.Split(raw, "\n")
	entries := make([]Entry, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		at, message := splitStamp(line)
		entries = append(entries, Entry{At: at, Message: message})
	}
	return entries
}

// splitStamp peels the RFC3339 timestamp the kubelet prefixes off a line.
func splitStamp(line string) (at, message string) {
	space := strings.IndexByte(line, ' ')
	if space <= 0 || !looksLikeStamp(line[:space]) {
		return "", line
	}
	return line[:space], line[space+1:]
}

// looksLikeStamp is a shape check, not a parse: the timestamp is passed through
// to the caller unparsed, so the only question here is where the line starts.
func looksLikeStamp(s string) bool {
	return len(s) >= 20 && s[4] == '-' && s[7] == '-' && s[10] == 'T' &&
		strings.HasSuffix(s, "Z")
}

// The closed set of shapes redaction knows. High entropy alone is deliberately
// not one of them: it blanks legitimate output, and a log an agent cannot read
// is worse than one carrying a value the agent's own app printed (AC-6).
var patterns = []struct {
	re   *regexp.Regexp
	with string
}{
	// An Authorization header value, both schemes. This also covers a JWT
	// carried as a bearer token, whatever shape the token itself has.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 " + redactedMark},
	// A JWT standing on its own. The header segment of every JWT base64url
	// encodes a leading `{"`, which is `eyJ`: requiring it is what keeps an
	// ordinary dotted hostname or version string out of the match.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`), redactedMark},
	// A URL carrying user:password@. The scheme, the user, and the host all
	// survive, because they are usually the point of the line.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+):[^/\s@]+@`), "$1:" + redactedMark + "@"},
	// An AWS style access key id, including the temporary ASIA form.
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), redactedMark},
	// An assignment whose name contains key, token, secret, or password, in the
	// shell, properties, and JSON shapes all at once.
	{regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:key|token|secret|password)[A-Za-z0-9_.-]*"?\s*[:=]\s*)"?[^\s",;}]+"?`), "$1" + redactedMark},
}

// Redact blanks the known secret shapes in one line, plus any literal values the
// platform knows for certain are secret (today the registry credential it placed
// in the app's namespace itself).
//
// This is best effort and the tool's description says so: a secret in a shape
// this set does not know is returned in clear, and the honest mitigation is
// saying that rather than a stronger regular expression (spec 0006,
// Security model).
func Redact(line string, literals []string) string {
	// Literals first: an exact match is the only redaction that can be certain,
	// and running it before the patterns keeps a known value from being half
	// rewritten into a shape the patterns then miss.
	for _, lit := range literals {
		if len(lit) < minLiteral {
			continue
		}
		line = strings.ReplaceAll(line, lit, redactedMark)
	}
	for _, p := range patterns {
		line = p.re.ReplaceAllString(line, p.with)
	}
	return line
}

// RedactAll runs Redact over a block, which is always done before bounding so
// the reported sizes and counts describe what the caller actually receives
// (AC-3, AC-6).
func RedactAll(entries []Entry, literals []string) []Entry {
	out := make([]Entry, len(entries))
	for i, e := range entries {
		out[i] = Entry{At: e.At, Message: Redact(e.Message, literals)}
	}
	return out
}

// Bound trims an already redacted block to a line count and a byte ceiling,
// dropping the oldest first, and reports how many entries went.
//
// The newest entry is always kept, even when it alone exceeds the ceiling: an
// empty answer would read as an app that printed nothing, which is a different
// and worse claim than a truncated one.
func Bound(entries []Entry, maxLines, maxBytes int) (kept []Entry, dropped int) {
	start := 0
	if maxLines > 0 && len(entries) > maxLines {
		start = len(entries) - maxLines
	}

	size := 0
	first := len(entries) // walk back from the newest until the ceiling is hit
	for i := len(entries) - 1; i >= start; i-- {
		size += entrySize(entries[i])
		if size > maxBytes && i < len(entries)-1 {
			break
		}
		first = i
	}
	return entries[first:], first
}

// entrySize is how much of the ceiling one entry spends: both fields, because
// both are carried to the caller.
func entrySize(e Entry) int { return len(e.At) + len(e.Message) }
