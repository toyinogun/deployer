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
// AC-15 names: ingress only, the exact three peer groups, the exact ports, and
// no egress rule anywhere.
//
// The egress assertions are the load bearing half. Adding Egress to policyTypes
// denies the control plane's own path to the Kubernetes API server, which sits
// on node addresses no rule here names, and takes the platform down.
//
// It sits in this package beside buildspolicy_test.go, which pins the same shape
// for the build namespaces; the YAML has no Go package of its own to live in.
const controlPlanePolicyFile = "../../deploy/deployer-system-networkpolicy.yaml"

// The four k3s node addresses, in the order the file lists them. Adding a node
// to the cluster means adding it here and in the YAML together.
var controlPlaneNodeCIDRs = []string{
	"172.16.70.20/32",
	"172.16.70.21/32",
	"172.16.70.22/32",
	"172.16.70.23/32",
}

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
// Every port pinned here is the destination pod's own container port. Cilium
// translates a ClusterIP address in eBPF before policy is evaluated, so the
// platform Service's port 80 would permit nothing at all.
func TestTheControlPlaneIngressIsTheTailnetTheNodesAndTheBuilds(t *testing.T) {
	p := controlPlanePolicies(t)["control-plane-allow"]
	if len(p.Spec.Ingress) != 3 {
		t.Fatalf("ingress rules = %d, want exactly 3 (tailnet, nodes, builds)", len(p.Spec.Ingress))
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

	// The four nodes, as individual /32 entries: 8080 for kubelet probes against
	// the control plane pod, 5000 for probes against the registry pod and for
	// every containerd image pull, which arrives as the node rather than a pod.
	nodes := p.Spec.Ingress[1]
	if len(nodes.From) != len(controlPlaneNodeCIDRs) {
		t.Fatalf("node peers = %d, want exactly %d, one per k3s node", len(nodes.From), len(controlPlaneNodeCIDRs))
	}
	for i, want := range controlPlaneNodeCIDRs {
		block := nodes.From[i].IPBlock
		if block == nil {
			t.Fatalf("node peer[%d] = %+v, want an ipBlock", i, nodes.From[i])
		}
		if block.CIDR != want {
			t.Errorf("node peer[%d] CIDR = %q, want %q", i, block.CIDR, want)
		}
		// An except list on a /32 either does nothing or empties the peer, and
		// either way it means someone edited this without meaning to.
		if len(block.Except) != 0 {
			t.Errorf("node peer[%d] carries an except list %v, want none on a /32", i, block.Except)
		}
	}
	assertPorts(t, "nodes", nodes.Ports, map[corev1.Protocol][]int32{corev1.ProtocolTCP: {8080, 5000}})

	// The two build namespaces, mirroring the egress rules they already carry.
	builds := p.Spec.Ingress[2]
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

// assertIngressOnly pins policyTypes to Ingress alone.
func assertIngressOnly(t *testing.T, name string, p networkingv1.NetworkPolicy) {
	t.Helper()

	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("%s policy types = %v, want Ingress alone: naming Egress here cuts the platform off from the API server",
			name, p.Spec.PolicyTypes)
	}
}
