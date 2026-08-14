package web

import (
	"bytes"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/identity"
)

// TestTokensPageShowsEveryColumn pins the five facts AC-21 promises about a live
// token: name, prefix, created, last used and expiry. Created and expiry were the
// two the first build left off the table, and nothing but rendering catches that.
func TestTokensPageShowsEveryColumn(t *testing.T) {
	var buf bytes.Buffer
	err := pages["tokens"].ExecuteTemplate(&buf, "base", pageData{
		Shell: shell{SignedIn: true, Email: "someone@example.test", CSRF: "csrf-value"},
		Data: tokensPageData{Tokens: []identity.TokenView{
			{
				ID: "tok_1", Name: "laptop agent", Prefix: "dpl_abcd",
				CreatedAt: "2026-08-14T05:55:00Z", LastUsedAt: "2026-08-14T06:19:00Z",
				ExpiresAt: "2026-09-13T05:55:00Z",
			},
			{ID: "tok_2", Name: "never used", Prefix: "dpl_efgh", CreatedAt: "2026-08-14T05:56:00Z"},
		}},
	})
	if err != nil {
		t.Fatalf("rendering the tokens page: %v", err)
	}
	got := buf.String()

	for _, want := range []string{
		">Name<", ">Prefix<", ">Created<", ">Last used<", ">Expires<", // the five columns
		"laptop agent", "dpl_abcd",
		"14 Aug 2026, 05:55 UTC", // created
		"14 Aug 2026, 06:19 UTC", // last used
		"13 Sep 2026, 05:55 UTC", // expiry
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the tokens page does not show %q", want)
		}
	}
	// A token with neither a last use nor an expiry says so rather than rendering
	// two blank cells, and never leaks a raw value or a hash (AC-21).
	if n := strings.Count(got, ">never<"); n != 2 {
		t.Errorf("want 2 never cells for the unused, non expiring token, got %d", n)
	}
}
