package deploy_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/toyinogun/deployer/internal/deploy"
)

// The eight ranges the default blocked list means, written out rather than
// derived from AllowedPortRanges: a test that computed them the same way the
// code does would agree with itself, and a broken complement would pass (spec
// 0017, AC-14). They are copied from the spec's Feature design, so a change to
// the config default that the policy does not follow fails here.
var wantDefaultRanges = []struct{ port, endPort int32 }{
	{1, 24},
	{26, 3332},
	{3334, 4443},
	{4445, 5554},
	{5556, 7776},
	{7778, 9998},
	{10000, 14443},
	{14445, 65535},
}

func TestAllowPolicyBoundsThePublicPortsToTheComplementOfTheBlockedList(t *testing.T) {
	internet := deploy.AllowPolicy(policyInput()).Spec.Egress[1]

	if len(internet.Ports) != len(wantDefaultRanges)+1 {
		t.Fatalf("internet ports = %d entries, want %d TCP ranges plus one UDP entry",
			len(internet.Ports), len(wantDefaultRanges))
	}
	for i, want := range wantDefaultRanges {
		got := internet.Ports[i]
		if got.Protocol == nil || *got.Protocol != corev1.ProtocolTCP {
			t.Errorf("ports[%d] protocol = %v, want TCP stated explicitly", i, got.Protocol)
			continue
		}
		if got.Port == nil || got.Port.IntVal != want.port {
			t.Errorf("ports[%d] port = %v, want %d", i, got.Port, want.port)
		}
		if got.EndPort == nil || *got.EndPort != want.endPort {
			t.Errorf("ports[%d] endPort = %v, want %d", i, got.EndPort, want.endPort)
		}
	}
}

// NetworkPolicyPort.Protocol defaults to TCP when it is nil, so a UDP entry
// composed without it does not mean "all UDP", it means "all TCP on any port",
// and the whole bound above is reopened by one missing field (AC-3).
func TestThePublicEgressUDPEntryStatesItsProtocol(t *testing.T) {
	internet := deploy.AllowPolicy(policyInput()).Spec.Egress[1]

	last := internet.Ports[len(internet.Ports)-1]
	if last.Protocol == nil || *last.Protocol != corev1.ProtocolUDP {
		t.Fatalf("last port entry protocol = %v, want UDP stated explicitly", last.Protocol)
	}
	// Unset on both ends is what makes it every UDP port. UDP is unconditioned by
	// the blocked list on purpose: narrowing it would break HTTP/3 and time
	// synchronisation for no matching benefit.
	if last.Port != nil || last.EndPort != nil {
		t.Errorf("UDP entry = %+v, want no port bound at all", last)
	}
}

// A ports list applies to every peer under `to` in the same rule, so a second
// peer added to this rule later would silently inherit the port bound, which is
// a way to break an unrelated destination without touching a line of the design
// that put the bound there (AC-4).
func TestThePublicEgressRuleCarriesExactlyOnePeer(t *testing.T) {
	for _, p := range []*networkingv1.NetworkPolicy{
		deploy.AllowPolicy(policyInput()),
		deploy.AllowPolicy(deploy.Input{Slug: "hello-a1b2c3"}),
	} {
		if got := len(p.Spec.Egress[1].To); got != 1 {
			t.Errorf("public egress peers = %d, want exactly 1 so nothing else inherits the ports list", got)
		}
	}
}

// A blocked port is a port no range names, so the bound holding is the same
// statement as these ports appearing nowhere in the composed list.
func TestTheBlockedPortsAppearInNoAllowedRange(t *testing.T) {
	in := policyInput()
	internet := deploy.AllowPolicy(in).Spec.Egress[1]

	for _, blocked := range in.EgressBlockedPorts {
		for i, entry := range internet.Ports {
			if entry.Protocol == nil || *entry.Protocol != corev1.ProtocolTCP || entry.Port == nil {
				continue
			}
			end := entry.Port.IntVal
			if entry.EndPort != nil {
				end = *entry.EndPort
			}
			if blocked >= entry.Port.IntVal && blocked <= end {
				t.Errorf("blocked port %d is inside allowed range ports[%d] = %d-%d",
					blocked, i, entry.Port.IntVal, end)
			}
		}
	}
}

// The ports the spec promises stay open, checked from the other direction so a
// complement that is wrong in a way the literal ranges above happen to match
// still fails.
func TestTheOrdinaryPortsStayReachable(t *testing.T) {
	internet := deploy.AllowPolicy(policyInput()).Spec.Egress[1]

	for _, port := range []int32{80, 443, 587, 5432, 6379} {
		reachable := false
		for _, entry := range internet.Ports {
			if entry.Protocol == nil || *entry.Protocol != corev1.ProtocolTCP || entry.Port == nil {
				continue
			}
			end := entry.Port.IntVal
			if entry.EndPort != nil {
				end = *entry.EndPort
			}
			if port >= entry.Port.IntVal && port <= end {
				reachable = true
				break
			}
		}
		if !reachable {
			t.Errorf("TCP %d is in no allowed range, want it reachable", port)
		}
	}
}
