package config

import (
	"os"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

// The build namespace fences are static YAML applied by ArgoCD, so no Go code
// composes them and nothing else would notice a hand edit that opens one up.
// This parses the files into the real API types and pins every clause AC-15
// names: deny both ways, no ingress rule anywhere, and egress limited to
// CoreDNS, the deployer-system pods on 5000 and 8080, and the public internet.
// The except list is pinned separately against the config default (AC-20, see
// blockeddrift_test.go).
//
// There are two of them because there are two build namespaces: Paketo Jobs run
// in deployer-builds at `restricted`, and BuildKit Jobs in
// deployer-builds-dockerfile at `privileged` (spec 0009, AC-10). A NetworkPolicy
// only selects pods in its own namespace, so the second namespace needs its own
// copy of the pair, and the copy is the thing that can drift (AC-21). Every
// assertion below therefore runs over both files, and one more asserts that they
// hold the same rules as each other.
//
// They sit in this package because the drift test already reads the same files
// from here; the YAML has no Go package of its own to live beside.
var buildsPolicyFiles = []struct{ path, namespace string }{
	{"../../deploy/builds-networkpolicy.yaml", "deployer-builds"},
	{"../../deploy/builds-dockerfile-networkpolicy.yaml", "deployer-builds-dockerfile"},
}

func buildsPolicies(t *testing.T, path, namespace string) map[string]networkingv1.NetworkPolicy {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	byName := map[string]networkingv1.NetworkPolicy{}
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var p networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatalf("parsing a document of %s: %v", path, err)
		}
		if p.Kind != "NetworkPolicy" {
			t.Fatalf("%s holds a %s, want NetworkPolicy objects only", path, p.Kind)
		}
		// A policy in the wrong namespace polices nothing that runs here, which
		// looks exactly like a fence from the outside.
		if p.Namespace != namespace {
			t.Errorf("%s in %s is in namespace %q, want %s", p.Name, path, p.Namespace, namespace)
		}
		byName[p.Name] = p
	}
	if len(byName) != 2 {
		t.Fatalf("%s holds %d policies, want exactly default-deny and build-allow", path, len(byName))
	}
	return byName
}

// AC-15: the deny is the floor everything else is an exception to. A rule of any
// kind on it, or a non empty selector, leaves part of the namespace unpoliced.
func TestTheBuildNamespaceDeniesBothDirectionsByDefault(t *testing.T) {
	for _, f := range buildsPolicyFiles {
		t.Run(f.namespace, func(t *testing.T) {
			p, ok := buildsPolicies(t, f.path, f.namespace)["default-deny"]
			if !ok {
				t.Fatalf("%s has no default-deny policy", f.path)
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
		})
	}
}

// AC-15: nothing connects to a build pod, so an ingress rule on either policy is
// an opening with no purpose. This is the clause a well meaning edit adds back.
func TestTheBuildNamespaceAllowsNoIngressAtAll(t *testing.T) {
	for _, f := range buildsPolicyFiles {
		t.Run(f.namespace, func(t *testing.T) {
			for name, p := range buildsPolicies(t, f.path, f.namespace) {
				if len(p.Spec.Ingress) != 0 {
					t.Errorf("%s carries %d ingress rules, want none: nothing dials a build pod", name, len(p.Spec.Ingress))
				}
				if !hasBothPolicyTypes(p) {
					t.Errorf("%s policy types = %v, want both Ingress and Egress", name, p.Spec.PolicyTypes)
				}
			}
		})
	}
}

// AC-15: exactly three ways out, in order. The count is the assertion that
// matters most, because a fourth rule is how an exception gets smuggled in.
func TestTheBuildNamespaceEgressIsCoreDNSTheRegistryAndThePublicInternet(t *testing.T) {
	for _, f := range buildsPolicyFiles {
		t.Run(f.namespace, func(t *testing.T) {
			p := buildsPolicies(t, f.path, f.namespace)["build-allow"]
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
			// An allow list of two rather than the ranges an app namespace gets: a
			// build fetches dependencies and base images, which is HTTP and HTTPS,
			// and nothing else it does over the public internet is legitimate (spec
			// 0017, AC-9). Unported here would mean a build pod could still reach
			// port 25 while the apps it produces cannot.
			assertPorts(t, "internet", internet.Ports, map[corev1.Protocol][]int32{corev1.ProtocolTCP: {80, 443}})
		})
	}
}

// AC-21: the two fences are copies of each other, and the failure this shape
// invites is one of them being widened alone. The assertions above would still
// pass for a file that grew a fourth port or swapped a selector, as long as it
// grew it in a shape they do not name; this one fails on any difference at all.
//
// The spec is compared rather than the whole object because the namespace and
// the labels are exactly what is meant to differ.
func TestTheTwoBuildFencesHoldTheSameRules(t *testing.T) {
	first := buildsPolicies(t, buildsPolicyFiles[0].path, buildsPolicyFiles[0].namespace)
	second := buildsPolicies(t, buildsPolicyFiles[1].path, buildsPolicyFiles[1].namespace)

	for name, a := range first {
		b, ok := second[name]
		if !ok {
			t.Errorf("%s holds a %s policy and %s does not", buildsPolicyFiles[0].path, name, buildsPolicyFiles[1].path)
			continue
		}
		if !reflect.DeepEqual(a.Spec, b.Spec) {
			t.Errorf("the %s policy differs between the two build namespaces:\n%s: %+v\n%s: %+v\n"+
				"one fence was widened without the other, which is the drift AC-21 exists to catch",
				name, buildsPolicyFiles[0].path, a.Spec, buildsPolicyFiles[1].path, b.Spec)
		}
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
