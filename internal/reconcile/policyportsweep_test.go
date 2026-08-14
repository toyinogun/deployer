package reconcile_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/toyinogun/deployer/internal/deploy"
)

// The sweep and the deploy path compose deploy.Input from two independent
// literals, so a bound added to one and forgotten in the other sweeps an app
// namespace back to a weaker policy than a deploy would write. That is the whole
// retrofit path for spec 0017 (AC-11), and the fake clientset cannot tell you
// whether the cluster enforces the result, only whether the object was composed.
func TestPolicySweepCarriesThePortBound(t *testing.T) {
	w := setup(t)
	ctx := t.Context()
	w.clientset = fake.NewClientset(appNamespaceObject("app-old", "old"))

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).PolicySweep(ctx)

	p, err := w.clientset.NetworkingV1().NetworkPolicies("app-old").Get(ctx, deploy.AllowPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the swept allow policy: %v", err)
	}
	internet := p.Spec.Egress[1]
	if len(internet.Ports) == 0 {
		t.Fatal("the swept policy has no ports list, so an older namespace stays unbounded")
	}
	// The fixture blocks 25 and 3333, which the sweep must express as the ranges
	// around them rather than as the whole space.
	for _, blocked := range []int32{25, 3333} {
		for _, entry := range internet.Ports {
			if entry.Protocol == nil || *entry.Protocol != corev1.ProtocolTCP || entry.Port == nil {
				continue
			}
			end := entry.Port.IntVal
			if entry.EndPort != nil {
				end = *entry.EndPort
			}
			if blocked >= entry.Port.IntVal && blocked <= end {
				t.Errorf("swept policy leaves blocked port %d open in range %d-%d", blocked, entry.Port.IntVal, end)
			}
		}
	}
}
