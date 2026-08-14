package config

import (
	"strconv"
	"strings"
	"testing"
)

const blockedPortsKey = "DEPLOYER_APP_EGRESS_BLOCKED_PORTS"

func TestBlockedPortsDefault(t *testing.T) {
	c, err := Load(env(valid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int32{25, 3333, 4444, 5555, 7777, 9999, 14444}
	if len(c.AppEgressBlockedPorts) != len(want) {
		t.Fatalf("blocked ports = %v, want %v", c.AppEgressBlockedPorts, want)
	}
	for i, port := range want {
		if c.AppEgressBlockedPorts[i] != port {
			t.Errorf("blocked[%d] = %d, want %d", i, c.AppEgressBlockedPorts[i], port)
		}
	}
}

// Set but empty is the same string as unset to os.Getenv, so it has to mean the
// default rather than a refusal: a ConfigMap that omits the key still boots, and
// it boots bounded (AC-1).
func TestAnEmptyBlockedPortsMeansTheDefault(t *testing.T) {
	c, err := Load(env(withValid(map[string]string{blockedPortsKey: ""})))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.AppEgressBlockedPorts) != 7 || c.AppEgressBlockedPorts[0] != 25 {
		t.Errorf("blocked ports = %v, want the seven default ports", c.AppEgressBlockedPorts)
	}
}

func TestBlockedPortsOverride(t *testing.T) {
	c, err := Load(env(withValid(map[string]string{blockedPortsKey: "23, 25"})))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.AppEgressBlockedPorts) != 2 || c.AppEgressBlockedPorts[0] != 23 || c.AppEgressBlockedPorts[1] != 25 {
		t.Errorf("blocked ports = %v, want [23 25], trimmed", c.AppEgressBlockedPorts)
	}
}

// The complement function walks the list in order and assumes each entry is seen
// once, so the sort and the deduplication happen here rather than there (AC-1).
func TestBlockedPortsAreSortedAndDeduplicated(t *testing.T) {
	c, err := Load(env(withValid(map[string]string{blockedPortsKey: "3333,25,3333,25,80"})))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int32{25, 80, 3333}
	if len(c.AppEgressBlockedPorts) != len(want) {
		t.Fatalf("blocked ports = %v, want %v", c.AppEgressBlockedPorts, want)
	}
	for i, port := range want {
		if c.AppEgressBlockedPorts[i] != port {
			t.Errorf("blocked[%d] = %d, want %d", i, c.AppEgressBlockedPorts[i], port)
		}
	}
}

// everyPort is the one override that leaves an app no outbound TCP at all, which
// is a silent inversion of what the rule is for rather than a broken value.
func everyPort() string {
	all := make([]string, 0, lastPort)
	for p := firstPort; p <= lastPort; p++ {
		all = append(all, strconv.Itoa(p))
	}
	return strings.Join(all, ",")
}

// Every one of these is a value that would compose a policy quietly meaning
// something other than a bound, which is why the boot is where they are caught
// (AC-1).
func TestBlockedPortsRejectedAtBoot(t *testing.T) {
	for name, value := range map[string]string{
		"unparseable":      "banana",
		"port zero":        "0",
		"above the range":  "65536",
		"negative":         "-1",
		"not an integer":   "25.5",
		"nothing usable":   ",  ,",
		"empty complement": everyPort(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(withValid(map[string]string{blockedPortsKey: value})))
			if err == nil {
				t.Fatalf("want a boot failure for %q, got nil", name)
			}
			if !strings.Contains(err.Error(), blockedPortsKey) {
				t.Errorf("error does not name the variable: %v", err)
			}
		})
	}
}
