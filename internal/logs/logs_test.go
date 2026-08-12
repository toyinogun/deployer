package logs

import (
	"strings"
	"testing"
)

func TestClampTail(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		applied   int
		clamped   bool
	}{
		{"absent reads as unset", 0, DefaultTail, false},
		{"negative reads as unset", -5, DefaultTail, false},
		{"a value in range is honoured", 50, 50, false},
		{"the maximum itself is not clamped", MaxTail, MaxTail, false},
		{"above the maximum is clamped, never refused", 5000, MaxTail, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			applied, clamped := ClampTail(tc.requested)
			if applied != tc.applied || clamped != tc.clamped {
				t.Fatalf("ClampTail(%d) = (%d, %v), want (%d, %v)",
					tc.requested, applied, clamped, tc.applied, tc.clamped)
			}
		})
	}
}

func TestParse(t *testing.T) {
	raw := "2026-08-12T10:00:00.000000000Z listening on :8080\n" +
		"2026-08-12T10:00:01.500000000Z served GET /\n"
	got := Parse(raw)
	if len(got) != 2 {
		t.Fatalf("Parse returned %d entries, want 2", len(got))
	}
	if got[0].At != "2026-08-12T10:00:00.000000000Z" || got[0].Message != "listening on :8080" {
		t.Fatalf("first entry = %+v", got[0])
	}
	if got[1].Message != "served GET /" {
		t.Fatalf("second entry = %+v", got[1])
	}
}

func TestParseKeepsALineWithNoTimestamp(t *testing.T) {
	// A line the kubelet did not stamp is still the app's output, so it is kept
	// with an empty timestamp rather than dropped or mangled.
	got := Parse("no timestamp here\n")
	if len(got) != 1 || got[0].At != "" || got[0].Message != "no timestamp here" {
		t.Fatalf("Parse gave %+v", got)
	}
}

func TestParseIgnoresBlankTail(t *testing.T) {
	if got := Parse(""); len(got) != 0 {
		t.Fatalf("Parse(\"\") returned %d entries, want 0", len(got))
	}
	if got := Parse("2026-08-12T10:00:00Z one\n\n"); len(got) != 1 {
		t.Fatalf("a trailing newline produced %d entries, want 1", len(got))
	}
}

func TestRedact(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		gone  string // must not appear in the result, "\x00" to assert nothing goes
		stays string // must survive, empty to skip
	}{
		{
			name:  "a bearer token in an Authorization header",
			line:  `GET /x Authorization: Bearer abcdef0123456789abcdef`,
			gone:  "abcdef0123456789abcdef",
			stays: "GET /x",
		},
		{
			name: "a basic credential",
			line: `Authorization: Basic dXNlcjpwYXNzd29yZA==`,
			gone: "dXNlcjpwYXNzd29yZA==",
		},
		{
			name:  "a JWT standing on its own",
			line:  `presented eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk to the api`,
			gone:  "dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			stays: "to the api",
		},
		{
			name:  "a URL carrying a password",
			line:  `dialing postgres://admin:hunter2@db.internal:5432/app`,
			gone:  "hunter2",
			stays: "db.internal:5432",
		},
		{
			name: "an AWS style access key id",
			line: `using AKIAIOSFODNN7EXAMPLE for the bucket`,
			gone: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:  "an assignment whose name says secret",
			line:  `STRIPE_SECRET_KEY=sk_live_51H8xQ2abcdef`,
			gone:  "sk_live_51H8xQ2abcdef",
			stays: "STRIPE_SECRET_KEY",
		},
		{
			name:  "a JSON field whose name says password",
			line:  `{"user":"ada","password":"correcthorse"}`,
			gone:  "correcthorse",
			stays: `"user":"ada"`,
		},
		{
			name:  "a long line that is merely long",
			line:  strings.Repeat("a", 120),
			gone:  "\x00",
			stays: strings.Repeat("a", 120),
		},
		{
			name:  "an ordinary hostname is not read as a JWT",
			line:  "resolved api.example.com in 3ms",
			gone:  "\x00",
			stays: "api.example.com",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.line, nil)
			if tc.gone != "\x00" && strings.Contains(got, tc.gone) {
				t.Fatalf("Redact left %q in place:\n%s", tc.gone, got)
			}
			if tc.stays != "" && !strings.Contains(got, tc.stays) {
				t.Fatalf("Redact removed %q, which should survive:\n%s", tc.stays, got)
			}
		})
	}
}

func TestRedactLiterals(t *testing.T) {
	// The registry credential the platform itself placed in the namespace: the
	// only redaction that can be exact.
	got := Redact("pulling with s3cr3t-registry-pass ok", []string{"s3cr3t-registry-pass"})
	if strings.Contains(got, "s3cr3t-registry-pass") {
		t.Fatalf("the literal survived: %s", got)
	}
	if !strings.Contains(got, "pulling with") {
		t.Fatalf("Redact ate the surrounding line: %s", got)
	}
}

func TestRedactIgnoresShortLiterals(t *testing.T) {
	// A short or empty literal would blank ordinary output everywhere it appears,
	// so it is not a redaction target at all.
	got := Redact("a run of ok results", []string{"", "ok"})
	if got != "a run of ok results" {
		t.Fatalf("a short literal changed the line: %s", got)
	}
}

func TestRedactAllLeavesTheInputAlone(t *testing.T) {
	// covers: AC-6. RedactAll runs before Bound, so the block it returns is what
	// the caller receives and its sizes are measured on. It builds a new block
	// rather than rewriting the parsed one in place, and every timestamp survives.
	in := []Entry{
		{At: "t1", Message: "Authorization: Bearer abcdefghijklmnop"},
		{At: "t2", Message: "started ok"},
	}
	out := RedactAll(in, nil)

	if in[0].Message != "Authorization: Bearer abcdefghijklmnop" {
		t.Fatalf("RedactAll rewrote its input: %s", in[0].Message)
	}
	if strings.Contains(out[0].Message, "abcdefghijklmnop") {
		t.Fatalf("the token survived: %s", out[0].Message)
	}
	if out[1].Message != "started ok" {
		t.Errorf("an ordinary line was changed: %s", out[1].Message)
	}
	if out[0].At != "t1" || out[1].At != "t2" {
		t.Errorf("timestamps did not survive: %+v", out)
	}
}

func TestBoundKeepsTheNewest(t *testing.T) {
	entries := []Entry{
		{At: "t1", Message: "one"},
		{At: "t2", Message: "two"},
		{At: "t3", Message: "three"},
	}
	kept, dropped := Bound(entries, 2, 1<<20)
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if len(kept) != 2 || kept[0].Message != "two" || kept[1].Message != "three" {
		t.Fatalf("Bound kept the wrong end: %+v", kept)
	}
}

func TestBoundByBytes(t *testing.T) {
	entries := []Entry{
		{At: "t1", Message: strings.Repeat("a", 100)},
		{At: "t2", Message: strings.Repeat("b", 100)},
		{At: "t3", Message: strings.Repeat("c", 100)},
	}
	kept, dropped := Bound(entries, 10, 220)
	if dropped != 1 || len(kept) != 2 {
		t.Fatalf("Bound gave %d kept, %d dropped, want 2 and 1", len(kept), dropped)
	}
	if kept[0].Message[0] != 'b' {
		t.Fatalf("Bound dropped the newest instead of the oldest: %+v", kept)
	}
}

func TestBoundUnderTheCeilingDropsNothing(t *testing.T) {
	entries := []Entry{{At: "t1", Message: "one"}}
	kept, dropped := Bound(entries, 10, 1<<20)
	if dropped != 0 || len(kept) != 1 {
		t.Fatalf("Bound trimmed an answer that fit: %d kept, %d dropped", len(kept), dropped)
	}
}

func TestBoundOnOneOversizeEntry(t *testing.T) {
	// A single entry larger than the whole ceiling still comes back, because an
	// empty answer would read as an app that printed nothing.
	entries := []Entry{{At: "t1", Message: strings.Repeat("a", 500)}}
	kept, dropped := Bound(entries, 10, 100)
	if len(kept) != 1 || dropped != 0 {
		t.Fatalf("Bound gave %d kept, %d dropped, want 1 and 0", len(kept), dropped)
	}
}
