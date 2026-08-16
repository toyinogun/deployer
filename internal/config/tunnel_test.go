package config

import (
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

// The tunnel's routing rules and the fence around its namespace are static YAML
// applied by ArgoCD, composed by no Go code and read by nothing at run time. This
// pins them the way controlplanepolicy_test.go pins the control plane's fence.
//
// What is public is decided entirely by the routing ConfigMap, so it is the one
// file in this repository where a careless edit publishes something. Hence the
// count assertions: an extra hostname is exactly the change that would otherwise
// go unnoticed.
const (
	tunnelConfigFile = "../../deploy/cloudflared-configmap.yaml"
	tunnelPolicyFile = "../../deploy/cloudflared-networkpolicy.yaml"
)

// tunnelIngress is one routing rule out of the ConfigMap. The fields are the
// ones the assertions below read; cloudflared accepts more and none of them are
// in use here.
type tunnelIngress struct {
	Hostname      string `json:"hostname"`
	Service       string `json:"service"`
	OriginRequest struct {
		OriginServerName string `json:"originServerName"`
		NoTLSVerify      *bool  `json:"noTLSVerify"`
	} `json:"originRequest"`
}

// tunnelConfig reads the routing rules out of the ConfigMap's embedded document.
// A minimal local struct rather than cloudflared's own types, for the same
// reason nodepolicy_test.go declines to import the Cilium module: it would add a
// dependency to go.mod for one test file.
func tunnelConfig(t *testing.T) struct {
	Tunnel  string          `json:"tunnel"`
	Metrics string          `json:"metrics"`
	Ingress []tunnelIngress `json:"ingress"`
} {
	t.Helper()

	var out struct {
		Tunnel  string          `json:"tunnel"`
		Metrics string          `json:"metrics"`
		Ingress []tunnelIngress `json:"ingress"`
	}
	raw, err := os.ReadFile(tunnelConfigFile)
	if err != nil {
		t.Fatalf("reading %s: %v", tunnelConfigFile, err)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("parsing %s: %v", tunnelConfigFile, err)
	}
	if cm.Namespace != "deployer-edge" {
		t.Errorf("%s is in namespace %q, want deployer-edge: the control plane's fence names that namespace as its peer",
			tunnelConfigFile, cm.Namespace)
	}
	body, ok := cm.Data["config.yaml"]
	if !ok {
		t.Fatalf("%s has no config.yaml key, so the tunnel would start with no routes at all", tunnelConfigFile)
	}
	if err := yaml.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("parsing the embedded config.yaml: %v", err)
	}
	return out
}

// AC-12: exactly two hostnames and a catch all that refuses. No other name
// reaches the cluster through this tunnel, and anything that becomes public is a
// deliberate edit to a reviewed file.
func TestTheTunnelRoutesExactlyTwoHostnames(t *testing.T) {
	cfg := tunnelConfig(t)
	if len(cfg.Ingress) != 3 {
		t.Fatalf("the tunnel carries %d rules, want exactly 3 (the apps, the console, the catch all)",
			len(cfg.Ingress))
	}

	var named int
	for i, rule := range cfg.Ingress[:2] {
		if rule.Hostname == "" {
			t.Errorf("rule[%d] names no hostname, so it catches everything before the rules after it", i)
			continue
		}
		named++
	}
	if named != 2 {
		t.Errorf("the tunnel names %d hostnames, want 2", named)
	}

	// The catch all is last and refuses. cloudflared requires a final rule with
	// no hostname; making it anything but a refusal publishes every name pointed
	// at this tunnel.
	last := cfg.Ingress[len(cfg.Ingress)-1]
	if last.Hostname != "" {
		t.Errorf("the last rule names hostname %q, want the catch all", last.Hostname)
	}
	if !strings.HasPrefix(last.Service, "http_status:4") && !strings.HasPrefix(last.Service, "http_status:5") {
		t.Errorf("the catch all serves %q, want a refusal: anything else publishes every name pointed here",
			last.Service)
	}
}

// tunnelRules returns the app wildcard rule and the console rule, found by shape
// rather than by position. Position is exactly what this file got wrong once, and
// reading the rules by index is what let every assertion here agree with a broken
// order: see TestNoTunnelRuleIsShadowedByAnEarlierOne.
func tunnelRules(t *testing.T) (apps, console tunnelIngress) {
	t.Helper()
	for _, rule := range tunnelConfig(t).Ingress {
		switch {
		case strings.HasPrefix(rule.Hostname, "*."):
			apps = rule
		case rule.Hostname != "":
			console = rule
		}
	}
	if apps.Hostname == "" {
		t.Fatal("no rule names an app wildcard hostname")
	}
	if console.Hostname == "" {
		t.Fatal("no rule names the console hostname")
	}
	return apps, console
}

// tunnelHostnameShadows reports whether an earlier cloudflared rule's hostname
// pattern already matches a later rule's hostname, which makes the later rule
// unreachable. A leading `*.` matches exactly one label, and an empty pattern is
// the catch all, which matches everything.
func tunnelHostnameShadows(pattern, host string) bool {
	if pattern == "" || pattern == host {
		return true
	}
	suffix, ok := strings.CutPrefix(pattern, "*.")
	if !ok {
		return false
	}
	label, rest, found := strings.Cut(host, ".")
	return found && label != "" && rest == suffix
}

// AC-9: cloudflared reads ingress rules top to bottom and takes the first
// hostname that matches, so a broader pattern listed first silently swallows
// every rule under it.
//
// This is not hypothetical. `*.deploy.toyintest.org` matches
// `console.deploy.toyintest.org`, because `console` is exactly one label, and the
// wildcard shipped above the console rule on 2026-08-16. Every console request
// went to ingress-nginx, which has no server block for that name and answered its
// own 404, so the console was unreachable through the tunnel while the app
// wildcard worked perfectly. Nothing here caught it: the assertions read
// Ingress[0] and Ingress[1] by position, and the two positions were wrong
// together, which reads as consistent.
func TestNoTunnelRuleIsShadowedByAnEarlierOne(t *testing.T) {
	rules := tunnelConfig(t).Ingress
	for i, rule := range rules {
		if rule.Hostname == "" {
			continue // the catch all is meant to be last and to match everything
		}
		for j, earlier := range rules[:i] {
			if tunnelHostnameShadows(earlier.Hostname, rule.Hostname) {
				t.Errorf("rule[%d] (%s) is never reached: rule[%d] (%q) already matches it, so that "+
					"hostname is served by the wrong origin", i, rule.Hostname, j, earlier.Hostname)
			}
		}
	}
}

// AC-9 and AC-9a: two rules, two different origins, and the app route verified
// against a name the wildcard certificate covers.
//
// The two mechanisms are separate and conflating them is how an origin looks
// verified while serving the wrong thing. Which backend serves a request is
// decided by the Host header, which cloudflared passes through unchanged.
// originServerName only proves to cloudflared that its TLS peer holds a
// certificate for that name.
func TestTheTunnelSendsTheAppsAndTheConsoleToDifferentOrigins(t *testing.T) {
	apps, console := tunnelRules(t)

	if !strings.HasPrefix(apps.Service, "https://") {
		t.Errorf("the app route serves %q, want https: tailnet traffic already reaches nginx over TLS "+
			"and the public path must not be weaker", apps.Service)
	}
	if !strings.Contains(apps.Service, "ingress-nginx") {
		t.Errorf("the app route serves %q, want the shared ingress controller", apps.Service)
	}
	if apps.OriginRequest.OriginServerName == "" {
		t.Error("the app route sets no originServerName, so cloudflared verifies against the Service's own " +
			"name, which the wildcard certificate does not cover and the route fails closed")
	}
	if strings.HasPrefix(apps.OriginRequest.OriginServerName, "*.") {
		t.Errorf("originServerName is %q, want the bare app domain: a literal asterisk is not a name "+
			"any certificate carries", apps.OriginRequest.OriginServerName)
	}
	if want := strings.TrimPrefix(apps.Hostname, "*."); apps.OriginRequest.OriginServerName != want {
		t.Errorf("originServerName = %q, want %q, the bare domain the wildcard certificate covers",
			apps.OriginRequest.OriginServerName, want)
	}

	// The console skips ingress-nginx entirely and goes straight to the control
	// plane Service. That is what lets network policy prove a request on this
	// hostname came through the tunnel, and therefore what lets the platform
	// trust CF-Connecting-IP there (AC-15a).
	if strings.Contains(console.Service, "ingress-nginx") {
		t.Errorf("the console route serves %q, want the deployer Service directly: behind the shared "+
			"controller the visitor address header would be writable from most of the cluster",
			console.Service)
	}
	if !strings.Contains(console.Service, "deployer.deployer-system") {
		t.Errorf("the console route serves %q, want the deployer Service in deployer-system", console.Service)
	}
	if apps.Service == console.Service {
		t.Error("both routes share one origin, so the console is not reaching the control plane directly")
	}
}

// AC-9: origin verification stays on for the app route. noTLSVerify would make a
// wrong certificate pass silently, which is the failure this route is meant to
// fail closed on.
func TestTheTunnelNeverSkipsOriginVerification(t *testing.T) {
	for i, rule := range tunnelConfig(t).Ingress {
		if rule.OriginRequest.NoTLSVerify != nil && *rule.OriginRequest.NoTLSVerify {
			t.Errorf("rule[%d] (%s) sets noTLSVerify, so a wrong certificate at the origin passes silently",
				i, rule.Hostname)
		}
	}
}

// AC-1: the console hostname in the tunnel's routes is the one the platform is
// configured with. They live in two files that are applied together, and a
// mismatch is a console that answers 404 through the tunnel while every test
// here passes.
func TestTheTunnelConsoleRouteMatchesTheConfiguredHost(t *testing.T) {
	apps, consoleRule := tunnelRules(t)
	console := consoleRule.Hostname
	raw, err := os.ReadFile("../../deploy/configmap.yaml")
	if err != nil {
		t.Fatalf("reading the platform ConfigMap: %v", err)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("parsing the platform ConfigMap: %v", err)
	}
	if got := cm.Data["DEPLOYER_CONSOLE_HOST"]; got != console {
		t.Errorf("DEPLOYER_CONSOLE_HOST is %q but the tunnel routes %q: the console would answer 404 "+
			"through the tunnel", got, console)
	}
	// The same pairing for the app domain, which the app route's hostname and
	// originServerName are both built from.
	if got, want := cm.Data["DEPLOYER_APP_DOMAIN"], apps.OriginRequest.OriginServerName; got != want {
		t.Errorf("DEPLOYER_APP_DOMAIN is %q but the tunnel verifies its origin against %q", got, want)
	}
}

func tunnelPolicies(t *testing.T) map[string]networkingv1.NetworkPolicy {
	t.Helper()

	raw, err := os.ReadFile(tunnelPolicyFile)
	if err != nil {
		t.Fatalf("reading %s: %v", tunnelPolicyFile, err)
	}
	byName := map[string]networkingv1.NetworkPolicy{}
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var p networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatalf("parsing a document of %s: %v", tunnelPolicyFile, err)
		}
		if p.Namespace != "deployer-edge" {
			t.Errorf("%s is in namespace %q, want deployer-edge", p.Name, p.Namespace)
		}
		byName[p.Name] = p
	}
	if len(byName) != 2 {
		t.Fatalf("%s holds %d policies, want default-deny and cloudflared-allow", tunnelPolicyFile, len(byName))
	}
	return byName
}

// AC-22a: the tunnel namespace is fenced in both directions.
//
// Both, unlike the control plane's fence, which is ingress only because policing
// its egress cuts it off from the Kubernetes API server. cloudflared needs no API
// server access at all, so nothing is lost by fencing it fully and a great deal
// would be lost by leaving its egress open: it is the one workload here whose job
// is talking to the internet.
func TestTheTunnelNamespaceIsFencedBothWays(t *testing.T) {
	for name, p := range tunnelPolicies(t) {
		if len(p.Spec.PolicyTypes) != 2 {
			t.Errorf("%s policy types = %v, want both Ingress and Egress", name, p.Spec.PolicyTypes)
			continue
		}
		var ingress, egress bool
		for _, pt := range p.Spec.PolicyTypes {
			ingress = ingress || pt == networkingv1.PolicyTypeIngress
			egress = egress || pt == networkingv1.PolicyTypeEgress
		}
		if !ingress || !egress {
			t.Errorf("%s policy types = %v, want both Ingress and Egress", name, p.Spec.PolicyTypes)
		}
	}

	deny := tunnelPolicies(t)["default-deny"]
	if len(deny.Spec.Ingress) != 0 || len(deny.Spec.Egress) != 0 {
		t.Error("the default deny carries rules, so it is not the floor the allow policy is an exception to")
	}
	if len(deny.Spec.PodSelector.MatchLabels) != 0 || len(deny.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("the default deny's pod selector = %+v, want empty so it selects every pod",
			deny.Spec.PodSelector)
	}
}

// AC-22a: nothing connects in but the platform's health check, on the metrics
// port alone. cloudflared dials out, so it is never a backend for anything.
func TestOnlyTheControlPlaneReachesTheTunnel(t *testing.T) {
	p := tunnelPolicies(t)["cloudflared-allow"]
	if len(p.Spec.Ingress) != 1 {
		t.Fatalf("ingress rules = %d, want exactly 1 (the platform's health check)", len(p.Spec.Ingress))
	}
	rule := p.Spec.Ingress[0]
	if len(rule.From) != 1 || rule.From[0].NamespaceSelector == nil {
		t.Fatalf("the health check peer = %+v, want one namespace selector alone", rule.From)
	}
	if got := rule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "deployer-system" {
		t.Errorf("the health check peer namespace = %q, want deployer-system", got)
	}
	assertPorts(t, "the tunnel health check", rule.Ports,
		map[corev1.Protocol][]int32{corev1.ProtocolTCP: {2000}})
}

// AC-22a: the tunnel reaches DNS, nginx on 443, the control plane on 8080, and
// Cloudflare. Nothing else.
//
// The control plane port is the destination pod's own container port, never the
// Service's 80. Cilium translates a ClusterIP in eBPF before policy is evaluated,
// so a rule written against the Service port permits nothing at all. The same
// trap is flagged in the build namespace policies.
func TestTheTunnelReachesOnlyItsFourDestinations(t *testing.T) {
	p := tunnelPolicies(t)["cloudflared-allow"]
	if len(p.Spec.Egress) != 4 {
		t.Fatalf("egress rules = %d, want exactly 4 (DNS, nginx, the control plane, Cloudflare)",
			len(p.Spec.Egress))
	}
	for i, rule := range p.Spec.Egress {
		if len(rule.Ports) == 0 {
			t.Errorf("egress rule[%d] names no ports, so it opens every port at that destination", i)
		}
	}

	byNamespace := map[string]networkingv1.NetworkPolicyEgressRule{}
	var public *networkingv1.NetworkPolicyEgressRule
	for i, rule := range p.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil {
				byNamespace[peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]] = rule
			}
			if peer.IPBlock != nil {
				public = &p.Spec.Egress[i]
			}
		}
	}

	if _, ok := byNamespace["kube-system"]; !ok {
		t.Error("the tunnel cannot reach cluster DNS, so none of its origin names resolve")
	}
	assertPorts(t, "nginx", byNamespace["ingress-nginx"].Ports,
		map[corev1.Protocol][]int32{corev1.ProtocolTCP: {443}})
	assertPorts(t, "the control plane", byNamespace["deployer-system"].Ports,
		map[corev1.Protocol][]int32{corev1.ProtocolTCP: {8080}})

	if public == nil {
		t.Fatal("the tunnel has no route to Cloudflare, so no connector ever registers")
	}
	// The private ranges are excepted, so the one rule that reaches the open
	// internet cannot become a path back into the cluster or onto the LAN.
	for _, peer := range public.To {
		if peer.IPBlock == nil {
			continue
		}
		for _, want := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
			if !contains(peer.IPBlock.Except, want) {
				t.Errorf("the public egress rule does not except %s, so it is also a path back into the "+
					"cluster and onto the LAN", want)
			}
		}
	}
}

// contains reports whether the list holds want.
func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
