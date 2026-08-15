package config

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The node sourced half of the control plane fence is a CiliumNetworkPolicy
// applied by ArgoCD, so nothing at run time would notice a hand edit. This pins
// the shape AC-12a names: an empty endpointSelector, exactly two entity rules
// with exactly those ports, and no egress key at all.
//
// It reads into the minimal local struct below rather than importing the Cilium
// module, which would be a new dependency in go.mod for one test file.
const nodePolicyFile = "../../deploy/deployer-system-cilium-networkpolicy.yaml"

// ciliumPolicy declares only the fields the assertions below touch.
//
// Note the port type. cilium.io/v2 takes a port as a string where
// networking.k8s.io/v1 takes an integer, so copying the int32 assertion from
// controlplanepolicy_test.go silently decodes every port to zero and passes.
type ciliumPolicy struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		EndpointSelector map[string]any `json:"endpointSelector"`
		Ingress          []struct {
			FromEntities []string `json:"fromEntities"`
			ToPorts      []struct {
				Ports []struct {
					Port     string `json:"port"`
					Protocol string `json:"protocol"`
				} `json:"ports"`
			} `json:"toPorts"`
		} `json:"ingress"`
		Egress []map[string]any `json:"egress"`
	} `json:"spec"`
}

func nodePolicy(t *testing.T) ciliumPolicy {
	t.Helper()

	raw, err := os.ReadFile(nodePolicyFile)
	if err != nil {
		t.Fatalf("reading %s: %v", nodePolicyFile, err)
	}
	// One object, on purpose. A second document here would be a second thing a
	// reader has to hold to know who reaches this namespace.
	if n := len(strings.Split(string(raw), "\n---")); n != 1 {
		t.Fatalf("%s holds %d documents, want exactly one", nodePolicyFile, n)
	}
	var p ciliumPolicy
	if err := yaml.Unmarshal(raw, &p); err != nil {
		t.Fatalf("parsing %s: %v", nodePolicyFile, err)
	}
	return p
}

// AC-11: namespaced, never cluster scoped. The deployer AppProject whitelists
// five cluster scoped kinds and cilium.io is not among them, and ArgoCD
// validates the whole operation rather than the one object, so a
// CiliumClusterwideNetworkPolicy here would stop everything in deploy/ from
// applying while the app still read as healthy.
func TestTheNodePolicyIsNamespacedAndLandsInTheControlPlaneNamespace(t *testing.T) {
	p := nodePolicy(t)
	if p.APIVersion != "cilium.io/v2" {
		t.Errorf("apiVersion = %q, want cilium.io/v2", p.APIVersion)
	}
	if p.Kind != "CiliumNetworkPolicy" {
		t.Errorf("kind = %q, want CiliumNetworkPolicy: a clusterwide one sits OutOfSync and blocks the whole sync", p.Kind)
	}
	if p.Metadata.Namespace != "deployer-system" {
		t.Errorf("namespace = %q, want deployer-system", p.Metadata.Namespace)
	}
}

// AC-12a: the selector covers every pod here, because the kubelet probes both
// and containerd pulls from the registry one. A label added to it would quietly
// drop whichever pod does not carry it.
func TestTheNodePolicySelectsEveryPodInTheNamespace(t *testing.T) {
	if sel := nodePolicy(t).Spec.EndpointSelector; len(sel) != 0 {
		t.Errorf("endpointSelector = %+v, want empty so it covers both pods", sel)
	}
}

// AC-12a: exactly two entity rules, exactly those ports.
//
// host is the kubelet's probes, which always arrive from the pod's own node.
// remote-node is containerd pulling an app image from whichever node the new pod
// landed on, and it gets 5000 alone: nothing needs it on 8080, and these two are
// as wide as the fence gets, since node sourced traffic carries no finer
// identity to select on.
func TestTheNodePolicyAdmitsHostAndRemoteNodeAndNothingElse(t *testing.T) {
	p := nodePolicy(t)
	if len(p.Spec.Ingress) != 2 {
		t.Fatalf("ingress rules = %d, want exactly 2 (host, remote-node)", len(p.Spec.Ingress))
	}

	want := []struct {
		entity string
		ports  []string
	}{
		{"host", []string{"8080", "5000"}},
		{"remote-node", []string{"5000"}},
	}
	for i, w := range want {
		rule := p.Spec.Ingress[i]
		if len(rule.FromEntities) != 1 || rule.FromEntities[0] != w.entity {
			t.Fatalf("ingress[%d] entities = %v, want %q alone", i, rule.FromEntities, w.entity)
		}
		if len(rule.ToPorts) != 1 {
			t.Fatalf("%s toPorts = %d clauses, want exactly 1", w.entity, len(rule.ToPorts))
		}
		got := rule.ToPorts[0].Ports
		if len(got) != len(w.ports) {
			t.Fatalf("%s names %d ports, want %d (%v)", w.entity, len(got), len(w.ports), w.ports)
		}
		for j, wantPort := range w.ports {
			// An empty port here is the string/int trap: a v1 style integer
			// decodes to "" and the rule opens every port for the entity.
			if got[j].Port != wantPort {
				t.Errorf("%s port[%d] = %q, want %q", w.entity, j, got[j].Port, wantPort)
			}
			if got[j].Protocol != "TCP" {
				t.Errorf("%s port[%d] protocol = %q, want TCP", w.entity, j, got[j].Protocol)
			}
		}
	}
}

// AC-11: ingress only, like its v1 sibling. An egress rule here would deny the
// control plane's own path to the Kubernetes API server, which sits on node
// addresses this fence names nowhere, and that is a full outage.
func TestTheNodePolicyNeverPolicesEgress(t *testing.T) {
	if e := nodePolicy(t).Spec.Egress; len(e) != 0 {
		t.Errorf("egress rules = %d, want none: this fence is ingress only", len(e))
	}
}
