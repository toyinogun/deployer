package config

import (
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

// The build namespace fence is static YAML applied by ArgoCD, so no Go code
// composes it and nothing else would notice a hand edit that opens it up. This
// parses the file into the real API types and pins every clause AC-15 names:
// deny both ways, no ingress rule anywhere, and egress limited to CoreDNS, the
// deployer-system pods on 5000 and 8080, and the public internet. The except
// list is pinned separately against the config default (AC-20, see
// blockeddrift_test.go).
//
// It sits in this package because the drift test already reads the same file
// from here; the YAML has no Go package of its own to live beside.
const buildsPolicyPath = "../../deploy/builds-networkpolicy.yaml"

func buildsPolicies(t *testing.T) map[string]networkingv1.NetworkPolicy {
	t.Helper()

	raw, err := os.ReadFile(buildsPolicyPath)
	if err != nil {
		t.Fatalf("reading %s: %v", buildsPolicyPath, err)
	}
	byName := map[string]networkingv1.NetworkPolicy{}
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var p networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatalf("parsing a document of %s: %v", buildsPolicyPath, err)
		}
		if p.Kind != "NetworkPolicy" {
			t.Fatalf("%s holds a %s, want NetworkPolicy objects only", buildsPolicyPath, p.Kind)
		}
		if p.Namespace != "deployer-builds" {
			t.Errorf("%s is in namespace %q, want deployer-builds", p.Name, p.Namespace)
		}
		byName[p.Name] = p
	}
	if len(byName) != 2 {
		t.Fatalf("%s holds %d policies, want exactly default-deny and build-allow", buildsPolicyPath, len(byName))
	}
	return byName
}

// AC-15: the deny is the floor everything else is an exception to. A rule of any
// kind on it, or a non empty selector, leaves part of the namespace unpoliced.
func TestTheBuildNamespaceDeniesBothDirectionsByDefault(t *testing.T) {
	p, ok := buildsPolicies(t)["default-deny"]
	if !ok {
		t.Fatalf("%s has no default-deny policy", buildsPolicyPath)
	}
	if len(p.Spec.PodSelector.MatchLabels) != 0 || len(p.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("pod selector = %+v, want empty so it selects every pod", p.Spec.PodSelector)
	}
	if !hasBothPolicyTypes(p) {
		t.Errorf("policy types = %v, want both Ingress and Egress", p.Spec.PolicyTypes)
	}
	if len(p.Spec.Ingress) != 0 || len(p.Spec.Egress) != 0 {
		t.Errorf("rules = %d ingress, %d egress, want none of either", len(p.Spec.Ingress), len(p.Spec.Egress))
	}
}

// AC-15: nothing connects to a build pod, so an ingress rule on either policy is
// an opening with no purpose. This is the clause a well meaning edit adds back.
func TestTheBuildNamespaceAllowsNoIngressAtAll(t *testing.T) {
	for name, p := range buildsPolicies(t) {
		if len(p.Spec.Ingress) != 0 {
			t.Errorf("%s carries %d ingress rules, want none: nothing dials a build pod", name, len(p.Spec.Ingress))
		}
		if !hasBothPolicyTypes(p) {
			t.Errorf("%s policy types = %v, want both Ingress and Egress", name, p.Spec.PolicyTypes)
		}
	}
}

// AC-15: exactly three ways out, in order. The count is the assertion that
// matters most, because a fourth rule is how an exception gets smuggled in.
func TestTheBuildNamespaceEgressIsCoreDNSTheRegistryAndThePublicInternet(t *testing.T) {
	p := buildsPolicies(t)["build-allow"]
	if len(p.Spec.Egress) != 3 {
		t.Fatalf("egress rules = %d, want exactly 3 (DNS, deployer-system, internet)", len(p.Spec.Egress))
	}

	// One peer carrying both selectors, not two peers. As two it would read
	// "anything in kube-system or anything labelled kube-dns anywhere", which is
	// the whole control plane namespace rather than the resolver.
	dns := p.Spec.Egress[0]
	if len(dns.To) != 1 {
		t.Fatalf("DNS peers = %d, want exactly 1 carrying both selectors", len(dns.To))
	}
	if dns.To[0].NamespaceSelector == nil || dns.To[0].PodSelector == nil {
		t.Fatalf("DNS peer = %+v, want a namespace selector and a pod selector together", dns.To[0])
	}
	if got := dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "kube-system" {
		t.Errorf("DNS namespace = %q, want kube-system", got)
	}
	if got := dns.To[0].PodSelector.MatchLabels["k8s-app"]; got != "kube-dns" {
		t.Errorf("DNS pod label k8s-app = %q, want kube-dns", got)
	}
	assertPorts(t, "DNS", dns.Ports, map[corev1.Protocol][]int32{corev1.ProtocolUDP: {53}, corev1.ProtocolTCP: {53}})

	// The registry and the control plane, on the destination pods' own ports:
	// Cilium translates a ClusterIP before policy runs, so the Service's 80
	// never appears here.
	cluster := p.Spec.Egress[1]
	if len(cluster.To) != 1 || cluster.To[0].NamespaceSelector == nil || cluster.To[0].PodSelector != nil {
		t.Fatalf("in cluster peer = %+v, want one namespace selector alone", cluster.To)
	}
	if got := cluster.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "deployer-system" {
		t.Errorf("in cluster namespace = %q, want deployer-system", got)
	}
	assertPorts(t, "deployer-system", cluster.Ports, map[corev1.Protocol][]int32{corev1.ProtocolTCP: {5000, 8080}})

	internet := p.Spec.Egress[2]
	if len(internet.To) != 1 || internet.To[0].IPBlock == nil {
		t.Fatalf("internet peer = %+v, want a single ipBlock", internet.To)
	}
	if got := internet.To[0].IPBlock.CIDR; got != "0.0.0.0/0" {
		t.Errorf("internet CIDR = %q, want 0.0.0.0/0", got)
	}
	if len(internet.To[0].IPBlock.Except) == 0 {
		t.Error("the internet rule excepts nothing, so it is an open door onto the LAN")
	}
}

func hasBothPolicyTypes(p networkingv1.NetworkPolicy) bool {
	seen := map[networkingv1.PolicyType]bool{}
	for _, t := range p.Spec.PolicyTypes {
		seen[t] = true
	}
	return len(p.Spec.PolicyTypes) == 2 && seen[networkingv1.PolicyTypeIngress] && seen[networkingv1.PolicyTypeEgress]
}

// assertPorts pins a rule's ports exactly: every wanted port present, and no
// port beyond them, since an extra one is a destination nobody reviewed.
func assertPorts(t *testing.T, rule string, got []networkingv1.NetworkPolicyPort, want map[corev1.Protocol][]int32) {
	t.Helper()

	found := map[corev1.Protocol][]int32{}
	for _, p := range got {
		if p.Protocol == nil || p.Port == nil {
			t.Fatalf("%s port %+v leaves protocol or port unset, which widens it", rule, p)
		}
		found[*p.Protocol] = append(found[*p.Protocol], p.Port.IntVal)
	}
	if len(found) != len(want) {
		t.Fatalf("%s ports = %v, want %v", rule, found, want)
	}
	for proto, ports := range want {
		if len(found[proto]) != len(ports) {
			t.Errorf("%s %s ports = %v, want %v", rule, proto, found[proto], ports)
			continue
		}
		for i, port := range ports {
			if found[proto][i] != port {
				t.Errorf("%s %s port[%d] = %d, want %d", rule, proto, i, found[proto][i], port)
			}
		}
	}
}
