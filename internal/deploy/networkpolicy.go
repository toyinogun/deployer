package deploy

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The network fence around one app (spec 0008). Two objects rather than one: a
// deny all that stands alone and says what the default is, and an allow that
// lists every exception in one readable place.
const (
	// DenyPolicyName selects every pod in the namespace and permits nothing.
	DenyPolicyName = "default-deny"

	// AllowPolicyName carries the three exceptions: ingress in, DNS out, and the
	// public internet out.
	AllowPolicyName = "app-allow"

	// ingressNamespace is where the ingress controller runs, matched on the
	// kubernetes.io/metadata.name label the API server sets itself, so it cannot
	// be forged by a workload or drift away from the namespace's name.
	ingressNamespace = "ingress-nginx"

	// dnsNamespace and dnsPodLabel identify CoreDNS. They are paired inside one
	// peer on purpose: two peers would mean "anything in kube-system OR anything
	// labelled kube-dns anywhere", which opens the whole namespace.
	dnsNamespace   = "kube-system"
	dnsPodLabelKey = "k8s-app"
	dnsPodLabel    = "kube-dns"

	// namespaceNameLabel is set and maintained by the API server on every
	// namespace, so selecting on it is selecting on the namespace's real name.
	namespaceNameLabel = "kubernetes.io/metadata.name"
)

// dnsPort is the one port CoreDNS is reached on, over both protocols.
const dnsPort = int32(53)

// DefaultDenyPolicy denies everything in and out of an app's namespace.
//
// An empty pod selector selects every pod, and a policy type with no rule under
// it permits nothing, so the whole object is the absence of rules (AC-2).
func DefaultDenyPolicy(in Input) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: objectMeta(DenyPolicyName, in.Slug),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
		},
	}
}

// AllowPolicy is every exception to the deny above, and nothing else: inbound
// from the ingress controller on the one port an app listens on, outbound to
// cluster DNS, and outbound to the public internet with the configured private
// ranges carved out (AC-3, AC-4, AC-5).
//
// Egress is allow by exception, so a new destination is reachable only by
// removing a range from that list, never by an app asking for it.
func AllowPolicy(in Input) *networkingv1.NetworkPolicy {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	appPort := intstr.FromInt32(ContainerPort)
	resolverPort := intstr.FromInt32(dnsPort)

	// Copied rather than aliased, so the composed object never shares backing
	// storage with the configuration every other app is composed from.
	except := make([]string, len(in.EgressBlockedCIDRs))
	copy(except, in.EgressBlockedCIDRs)

	return &networkingv1.NetworkPolicy{
		ObjectMeta: objectMeta(AllowPolicyName, in.Slug),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: matchNamespace(ingressNamespace),
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &appPort}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// One peer, both selectors: CoreDNS pods in kube-system, and
					// nothing else in either set.
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: matchNamespace(dnsNamespace),
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{dnsPodLabelKey: dnsPodLabel}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &resolverPort},
						{Protocol: &tcp, Port: &resolverPort},
					},
				},
				{
					// IPv4 only, on purpose: a dual stack cluster would need ::/0
					// added deliberately, and denied is the safe direction to be
					// wrong in (spec 0008, Consequences).
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: allEgressCIDR, Except: except},
					}},
				},
			},
		},
	}
}

// allEgressCIDR is every IPv4 address, which the except list above carves the
// cluster and the home network out of.
const allEgressCIDR = "0.0.0.0/0"

// matchNamespace selects one namespace by the name label the API server owns.
func matchNamespace(name string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{namespaceNameLabel: name}}
}
