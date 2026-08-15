package config

import (
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

// The control plane namespace fence is static YAML applied by ArgoCD, so no Go
// code composes it and nothing at run time would notice a hand edit that opens
// it up. This parses the file into the real API types and pins every clause
// AC-15 names: ingress only, the exact three peer groups, the exact ports, no
// node address anywhere, and no egress rule.
//
// The egress assertions are the load bearing half. Adding Egress to policyTypes
// denies the control plane's own path to the Kubernetes API server, which sits
// on node addresses no rule here names, and takes the platform down.
//
// The node sourced callers are not in this file. They are Cilium entities in
// deployer-system-cilium-networkpolicy.yaml, pinned by nodepolicy_test.go.
//
// It sits in this package beside buildspolicy_test.go, which pins the same shape
// for the build namespaces; the YAML has no Go package of its own to live in.
const controlPlanePolicyFile = "../../deploy/deployer-system-networkpolicy.yaml"

func controlPlanePolicies(t *testing.T) map[string]networkingv1.NetworkPolicy {
	t.Helper()

	raw, err := os.ReadFile(controlPlanePolicyFile)
	if err != nil {
		t.Fatalf("reading %s: %v", controlPlanePolicyFile, err)
	}
	byName := map[string]networkingv1.NetworkPolicy{}
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var p networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatalf("parsing a document of %s: %v", controlPlanePolicyFile, err)
		}
		if p.Kind != "NetworkPolicy" {
			t.Fatalf("%s holds a %s, want NetworkPolicy objects only", controlPlanePolicyFile, p.Kind)
		}
		// A policy in the wrong namespace polices nothing that runs here, which
		// looks exactly like a fence from the outside.
		if p.Namespace != "deployer-system" {
			t.Errorf("%s is in namespace %q, want deployer-system", p.Name, p.Namespace)
		}
		byName[p.Name] = p
	}
	if len(byName) != 2 {
		t.Fatalf("%s holds %d policies, want exactly default-deny and control-plane-allow", controlPlanePolicyFile, len(byName))
	}
	return byName
}

// AC-11: the deny is the floor the allow policy is an exception to. A rule of
// any kind on it, or a non empty selector, leaves part of the namespace
// unpoliced.
func TestTheControlPlaneNamespaceDeniesIngressByDefault(t *testing.T) {
	p, ok := controlPlanePolicies(t)["default-deny"]
	if !ok {
		t.Fatalf("%s has no default-deny policy", controlPlanePolicyFile)
	}
	if len(p.Spec.PodSelector.MatchLabels) != 0 || len(p.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("pod selector = %+v, want empty so it selects every pod", p.Spec.PodSelector)
	}
	assertIngressOnly(t, "default-deny", p)
	if len(p.Spec.Ingress) != 0 {
		t.Errorf("ingress rules = %d, want none: the allow policy carries the exceptions", len(p.Spec.Ingress))
	}
}

// AC-11: egress out of this namespace is deliberately untouched. Naming Egress
// under policyTypes on either policy denies the platform's own path to the
// Kubernetes API server, and that is an outage rather than a tightening.
func TestTheControlPlaneFenceNeverPolicesEgress(t *testing.T) {
	for name, p := range controlPlanePolicies(t) {
		assertIngressOnly(t, name, p)
		if len(p.Spec.Egress) != 0 {
			t.Errorf("%s carries %d egress rules, want none: this fence is ingress only", name, len(p.Spec.Egress))
		}
	}
}

// AC-12: exactly three ways in, in order. The count is the assertion that
// matters most, because a fourth rule is how an exception gets smuggled in.
//
// Three, not four: the node peer that used to sit second is gone. It was four
// /32 entries and it admitted nothing, because Cilium settles node traffic onto
// reserved identities before a CIDR rule is read. The nodes are now entities in
// deployer-system-cilium-networkpolicy.yaml, and this name changed with the
// assertion on purpose. Pinning the new shape under a name that still said
// "the nodes" would pass the suite and mislead the next reader, and nothing
// else catches that.
//
// Note what this test cannot do. A peer left out makes the list shorter, and a
// shorter list is a perfectly valid policy, so no assertion here reports one
// missing: only AC-14's live walk does. That is how the fourth peer came to be
// absent while this file was green.
//
// Every port pinned here is the destination pod's own container port. Cilium
// translates a ClusterIP address in eBPF before policy is evaluated, so the
// platform Service's port 80 would permit nothing at all.
func TestTheControlPlaneIngressIsTheTailnetTheBuildsAndItself(t *testing.T) {
	p := controlPlanePolicies(t)["control-plane-allow"]
	if len(p.Spec.Ingress) != 3 {
		t.Fatalf("ingress rules = %d, want exactly 3 (tailnet, builds, the control plane pod)", len(p.Spec.Ingress))
	}

	// The Tailscale ingress proxy, on 8080 alone. Console traffic never has
	// business reaching the registry on 5000.
	tailnet := p.Spec.Ingress[0]
	if len(tailnet.From) != 1 || tailnet.From[0].NamespaceSelector == nil || tailnet.From[0].PodSelector != nil {
		t.Fatalf("tailnet peer = %+v, want one namespace selector alone", tailnet.From)
	}
	if got := tailnet.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "tailscale" {
		t.Errorf("tailnet namespace = %q, want tailscale", got)
	}
	assertPorts(t, "tailnet", tailnet.Ports, map[corev1.Protocol][]int32{corev1.ProtocolTCP: {8080}})

	// The two build namespaces, mirroring the egress rules they already carry.
	builds := p.Spec.Ingress[1]
	wantBuilds := []string{"deployer-builds", "deployer-builds-dockerfile"}
	if len(builds.From) != len(wantBuilds) {
		t.Fatalf("build peers = %d, want exactly %d", len(builds.From), len(wantBuilds))
	}
	for i, want := range wantBuilds {
		sel := builds.From[i].NamespaceSelector
		if sel == nil || builds.From[i].PodSelector != nil {
			t.Fatalf("build peer[%d] = %+v, want one namespace selector alone", i, builds.From[i])
		}
		if got := sel.MatchLabels["kubernetes.io/metadata.name"]; got != want {
			t.Errorf("build peer[%d] namespace = %q, want %q", i, got, want)
		}
	}
	assertPorts(t, "builds", builds.Ports, map[corev1.Protocol][]int32{corev1.ProtocolTCP: {5000, 8080}})

	// The control plane pod itself, reading a pushed image back from the
	// registry on 5000. A namespaceSelector beside this podSelector would widen
	// it from this namespace to every namespace carrying the label, so its
	// absence is the assertion rather than an omission.
	self := p.Spec.Ingress[2]
	if len(self.From) != 1 || self.From[0].PodSelector == nil || self.From[0].NamespaceSelector != nil {
		t.Fatalf("in namespace peer = %+v, want one pod selector alone", self.From)
	}
	if got := self.From[0].PodSelector.MatchLabels["app"]; got != "deployer" {
		t.Errorf("in namespace peer app label = %q, want deployer", got)
	}
	assertPorts(t, "the control plane pod", self.Ports, map[corev1.Protocol][]int32{corev1.ProtocolTCP: {5000}})
}

// AC-12: a peer group with no ports clause permits every port in the namespace,
// which is how the console would reach the registry. Pinned on its own because
// dropping a ports clause is a deletion rather than an edit, and a deletion is
// the sort of change the shape assertions above can be talked into accepting.
func TestEveryControlPlaneIngressRuleCarriesItsOwnPorts(t *testing.T) {
	for i, rule := range controlPlanePolicies(t)["control-plane-allow"].Spec.Ingress {
		if len(rule.Ports) == 0 {
			t.Errorf("ingress rule[%d] names no ports, so it opens every port in the namespace", i)
		}
	}
}

// AC-12, AC-12a: no node address, anywhere in this file, ever again.
//
// Naming the four nodes as /32 ipBlocks here is the obvious thing to reach for
// and it admits nothing: Cilium settles node sourced traffic onto the reserved
// host and remote-node identities before a CIDR rule is evaluated. It looks
// right, it parses, and the only instrument that finds it is an image pull on a
// node that is not the registry's. So a revert to addresses fails here instead.
func TestTheControlPlaneFenceNamesNoAddresses(t *testing.T) {
	for name, p := range controlPlanePolicies(t) {
		for i, rule := range p.Spec.Ingress {
			for j, peer := range rule.From {
				if peer.IPBlock != nil {
					t.Errorf("%s ingress[%d].from[%d] is an ipBlock %q: node addresses match nothing under Cilium, "+
						"and the nodes belong in deployer-system-cilium-networkpolicy.yaml as entities",
						name, i, j, peer.IPBlock.CIDR)
				}
			}
		}
	}
}

// assertIngressOnly pins policyTypes to Ingress alone.
func assertIngressOnly(t *testing.T, name string, p networkingv1.NetworkPolicy) {
	t.Helper()

	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("%s policy types = %v, want Ingress alone: naming Egress here cuts the platform off from the API server",
			name, p.Spec.PolicyTypes)
	}
}
