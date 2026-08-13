package config

import (
	"strings"
	"testing"
	"time"
)

// TestTheReaperKnobsDefaultToTheSpecsValues pins the two defaults, which are what
// the platform runs on unless a ConfigMap says otherwise.
func TestTheReaperKnobsDefaultToTheSpecsValues(t *testing.T) {
	// covers: AC-23, AC-26
	c, err := Load(env(valid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.ReapInterval != 10*time.Minute {
		t.Errorf("reap interval = %s, want ten minutes", c.ReapInterval)
	}
	if c.OrphanGrace != 15*time.Minute {
		t.Errorf("orphan grace = %s, want fifteen minutes", c.OrphanGrace)
	}
}

// TestTheReaperKnobsAreValidatedAtStartup keeps a nonsense value from being
// discovered by the first pass instead of by the boot.
func TestTheReaperKnobsAreValidatedAtStartup(t *testing.T) {
	// covers: AC-26
	for key, value := range map[string]string{
		"DEPLOYER_REAP_INTERVAL_SECONDS": "0",
		"DEPLOYER_ORPHAN_GRACE_SECONDS":  "soon",
	} {
		if _, err := Load(env(withValid(map[string]string{key: value}))); err == nil {
			t.Errorf("%s=%q was accepted, want the boot refused", key, value)
		} else if !strings.Contains(err.Error(), key) {
			t.Errorf("the error for %s reads %q, want it to name the variable", key, err)
		}
	}
}

// TestTheReaperKnobsTakeAnOverride pins that a value in seconds is read as one.
func TestTheReaperKnobsTakeAnOverride(t *testing.T) {
	// covers: AC-23, AC-26
	c, err := Load(env(withValid(map[string]string{
		"DEPLOYER_REAP_INTERVAL_SECONDS": "60",
		"DEPLOYER_ORPHAN_GRACE_SECONDS":  "120",
	})))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.ReapInterval != time.Minute || c.OrphanGrace != 2*time.Minute {
		t.Errorf("reap interval = %s and grace = %s, want one and two minutes", c.ReapInterval, c.OrphanGrace)
	}
}
