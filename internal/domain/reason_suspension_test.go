package domain_test

import (
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
)

// TestAccountSuspendedIsInTheClosedSet is AC-15. A code that is not in the set
// reads as internal to every caller and would arrive at a person as the platform
// failing rather than as the decision it is. covers: AC-15
func TestAccountSuspendedIsInTheClosedSet(t *testing.T) {
	t.Parallel()
	if !domain.ReasonAccountSuspended.Valid() {
		t.Fatal("account_suspended is not a member of the closed reason set")
	}
	line := domain.ReasonAccountSuspended.Message()
	if line == domain.ReasonInternal.Message() {
		t.Error("account_suspended falls back to the internal line, so it carries no line of its own")
	}
	if line == "" {
		t.Error("account_suspended carries an empty line")
	}
	// The line names no internal detail: it says what the caller should do about
	// it, which is the whole reason the set is closed.
	if got := string(domain.ReasonAccountSuspended); got != "account_suspended" {
		t.Errorf("the code reads %q, want account_suspended", got)
	}
}
