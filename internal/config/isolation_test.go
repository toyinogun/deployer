package config

import (
	"strings"
	"testing"
)

const blockedKey = "DEPLOYER_APP_EGRESS_BLOCKED_CIDRS"

func TestBlockedCIDRsDefault(t *testing.T) {
	c, err := Load(env(valid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10"}
	if len(c.AppEgressBlockedCIDRs) != len(want) {
		t.Fatalf("blocked CIDRs = %v, want %v", c.AppEgressBlockedCIDRs, want)
	}
	for i, cidr := range want {
		if c.AppEgressBlockedCIDRs[i] != cidr {
			t.Errorf("blocked[%d] = %q, want %q", i, c.AppEgressBlockedCIDRs[i], cidr)
		}
	}
}

// Set but empty is the same string as unset to os.Getenv, so it has to mean the
// default rather than a refusal: a ConfigMap that omits the key still boots, and
// it boots fenced. A value that is set and leaves no usable entry is the case
// that gets refused, pinned in TestBlockedCIDRsRejectedAtBoot (AC-14).
func TestAnEmptyBlockedCIDRsMeansTheDefault(t *testing.T) {
	c, err := Load(env(withValid(map[string]string{blockedKey: ""})))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.AppEgressBlockedCIDRs) != 5 || c.AppEgressBlockedCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("blocked CIDRs = %v, want the five default ranges", c.AppEgressBlockedCIDRs)
	}
}

func TestBlockedCIDRsOverride(t *testing.T) {
	c, err := Load(env(withValid(map[string]string{blockedKey: "10.0.0.0/8, 192.168.0.0/16"})))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.AppEgressBlockedCIDRs) != 2 || c.AppEgressBlockedCIDRs[1] != "192.168.0.0/16" {
		t.Errorf("blocked CIDRs = %v, want the two given, trimmed", c.AppEgressBlockedCIDRs)
	}
}

// A hand typed override keeps whatever host bits the operator left on, and the
// value goes into an `except` entry the drift test compares byte for byte, so it
// is normalised on the way in rather than left to read back differently.
func TestBlockedCIDRsAreStoredMasked(t *testing.T) {
	c, err := Load(env(withValid(map[string]string{blockedKey: "10.0.0.5/8"})))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.AppEgressBlockedCIDRs) != 1 || c.AppEgressBlockedCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("blocked CIDRs = %v, want [10.0.0.0/8] with the host bits masked off", c.AppEgressBlockedCIDRs)
	}
}

// Every one of these parses cleanly somewhere else in the system and does the
// wrong thing here, which is why the boot is where they are caught (AC-14).
func TestBlockedCIDRsRejectedAtBoot(t *testing.T) {
	for name, value := range map[string]string{
		"not a CIDR":     "10.0.0.0",
		"unparseable":    "banana",
		"IPv6":           "10.0.0.0/8,fd00::/8",
		"nothing usable": ",  ,",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(withValid(map[string]string{blockedKey: value})))
			if err == nil {
				t.Fatalf("want a boot failure for %q, got nil", value)
			}
			if !strings.Contains(err.Error(), blockedKey) {
				t.Errorf("error does not name the variable: %v", err)
			}
		})
	}
}
