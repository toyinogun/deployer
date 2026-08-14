package deploy_test

import (
	"testing"

	"github.com/toyinogun/deployer/internal/deploy"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

// The shape of the two composed app policies (spec 0008, AC-1 through AC-5).
// The fake clientset enforces nothing, so these pin the objects rather than the
// behaviour: whether the fence actually holds is AC-6 to AC-9, and only the live
// cluster can answer that.
//
// policyInput is one app's composition with a blocked list distinct enough that
// a rule picking up the wrong one is visible.
func policyInput() deploy.Input {
	in := input()
	in.EgressBlockedCIDRs = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	in.EgressBlockedPorts = []int32{25, 3333, 4444, 5555, 7777, 9999, 14444}
	return in
}

func TestDefaultDenyPolicyDeniesBothDirections(t *testing.T) {
	p := deploy.DefaultDenyPolicy(policyInput())

	if p.Name != "default-deny" {
		t.Errorf("name = %q, want default-deny", p.Name)
	}
	if p.Namespace != "app-hello-a1b2c3" {
		t.Errorf("namespace = %q, want app-hello-a1b2c3", p.Namespace)
	}
	// An empty selector is what selects every pod. A non empty one would leave
	// whatever it does not match completely unpoliced.
	if len(p.Spec.PodSelector.MatchLabels) != 0 || len(p.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("pod selector = %+v, want empty so it selects every pod", p.Spec.PodSelector)
	}
	if !hasPolicyTypes(p, networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress) {
		t.Errorf("policy types = %v, want both Ingress and Egress", p.Spec.PolicyTypes)
	}
	// Rules are what a deny must not have: one would be an allow.
	if len(p.Spec.Ingress) != 0 || len(p.Spec.Egress) != 0 {
		t.Errorf("rules = %d ingress, %d egress, want none of either", len(p.Spec.Ingress), len(p.Spec.Egress))
	}
}

func TestAllowPolicyAdmitsOnlyTheIngressControllerOnTheAppPort(t *testing.T) {
	p := deploy.AllowPolicy(policyInput())

	if p.Name != "app-allow" {
		t.Errorf("name = %q, want app-allow", p.Name)
	}
	if !hasPolicyTypes(p, networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress) {
		t.Errorf("policy types = %v, want both Ingress and Egress", p.Spec.PolicyTypes)
	}
	if len(p.Spec.Ingress) != 1 {
		t.Fatalf("ingress rules = %d, want exactly 1", len(p.Spec.Ingress))
	}
	rule := p.Spec.Ingress[0]
	if len(rule.From) != 1 {
		t.Fatalf("ingress peers = %d, want exactly 1", len(rule.From))
	}
	peer := rule.From[0]
	if peer.IPBlock != nil || peer.PodSelector != nil {
		t.Errorf("ingress peer = %+v, want a namespace selector alone", peer)
	}
	if got := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "ingress-nginx" {
		t.Errorf("ingress from namespace = %q, want ingress-nginx", got)
	}
	if len(rule.Ports) != 1 {
		t.Fatalf("ingress ports = %d, want exactly 1", len(rule.Ports))
	}
	if got := rule.Ports[0].Port.IntVal; got != deploy.ContainerPort {
		t.Errorf("ingress port = %d, want %d", got, deploy.ContainerPort)
	}
	if got := *rule.Ports[0].Protocol; got != corev1.ProtocolTCP {
		t.Errorf("ingress protocol = %s, want TCP", got)
	}
}

// The DNS rule is the one an implementation gets subtly wrong: two peers instead
// of one means "anything in kube-system OR anything labelled kube-dns anywhere",
// which is the whole namespace rather than the resolver.
func TestAllowPolicyPairsTheDNSSelectorsInOnePeer(t *testing.T) {
	p := deploy.AllowPolicy(policyInput())

	if len(p.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want exactly 2 (DNS and the internet)", len(p.Spec.Egress))
	}
	dns := p.Spec.Egress[0]
	if len(dns.To) != 1 {
		t.Fatalf("DNS peers = %d, want exactly 1 carrying both selectors", len(dns.To))
	}
	peer := dns.To[0]
	if peer.NamespaceSelector == nil || peer.PodSelector == nil {
		t.Fatalf("DNS peer = %+v, want a namespace selector and a pod selector together", peer)
	}
	if got := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "kube-system" {
		t.Errorf("DNS namespace = %q, want kube-system", got)
	}
	if got := peer.PodSelector.MatchLabels["k8s-app"]; got != "kube-dns" {
		t.Errorf("DNS pod label k8s-app = %q, want kube-dns", got)
	}

	wantPorts := map[corev1.Protocol]bool{corev1.ProtocolUDP: false, corev1.ProtocolTCP: false}
	for _, port := range dns.Ports {
		if port.Port.IntVal != 53 {
			t.Errorf("DNS port = %d, want 53", port.Port.IntVal)
		}
		wantPorts[*port.Protocol] = true
	}
	for proto, seen := range wantPorts {
		if !seen {
			t.Errorf("DNS is not permitted over %s, want both UDP and TCP", proto)
		}
	}
}

func TestAllowPolicyExceptsEveryBlockedRangeFromTheInternetRule(t *testing.T) {
	in := policyInput()
	p := deploy.AllowPolicy(in)

	internet := p.Spec.Egress[1]
	// The ports half of this rule is spec 0017's, pinned in portpolicy_test.go.
	// What is asserted here is that adding it left the peer alone: the `except`
	// list decides which addresses, the ports list decides which ports on the ones
	// that are left, and the two compose without interacting.
	if len(internet.To) != 1 || internet.To[0].IPBlock == nil {
		t.Fatalf("internet peer = %+v, want a single ipBlock", internet.To)
	}
	block := internet.To[0].IPBlock
	if block.CIDR != "0.0.0.0/0" {
		t.Errorf("internet CIDR = %q, want 0.0.0.0/0", block.CIDR)
	}
	if len(block.Except) != len(in.EgressBlockedCIDRs) {
		t.Fatalf("except = %v, want %v", block.Except, in.EgressBlockedCIDRs)
	}
	for i, want := range in.EgressBlockedCIDRs {
		if block.Except[i] != want {
			t.Errorf("except[%d] = %q, want %q", i, block.Except[i], want)
		}
	}
}

// The composed object must not share backing storage with the configuration,
// or one app's policy could be edited through another's.
func TestAllowPolicyCopiesTheBlockedList(t *testing.T) {
	in := policyInput()
	p := deploy.AllowPolicy(in)

	in.EgressBlockedCIDRs[0] = "0.0.0.0/0"
	if got := p.Spec.Egress[1].To[0].IPBlock.Except[0]; got != "10.0.0.0/8" {
		t.Errorf("except[0] = %q after the input was edited, want the composed value 10.0.0.0/8", got)
	}
}

func TestPoliciesCarryTheOwnershipLabels(t *testing.T) {
	for _, p := range []*networkingv1.NetworkPolicy{
		deploy.DefaultDenyPolicy(policyInput()),
		deploy.AllowPolicy(policyInput()),
	} {
		if got := p.Labels["app.kubernetes.io/managed-by"]; got != "deployer" {
			t.Errorf("%s managed-by = %q, want deployer", p.Name, got)
		}
		if got := p.Labels["app.kubernetes.io/name"]; got != "hello-a1b2c3" {
			t.Errorf("%s name label = %q, want the slug", p.Name, got)
		}
	}
}

func hasPolicyTypes(p *networkingv1.NetworkPolicy, want ...networkingv1.PolicyType) bool {
	if len(p.Spec.PolicyTypes) != len(want) {
		return false
	}
	seen := map[networkingv1.PolicyType]bool{}
	for _, t := range p.Spec.PolicyTypes {
		seen[t] = true
	}
	for _, t := range want {
		if !seen[t] {
			return false
		}
	}
	return true
}
