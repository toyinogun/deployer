package reconcile_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/deploy"
	"github.com/toyinogun/deployer/internal/domain"
	"github.com/toyinogun/deployer/internal/kube"
	"github.com/toyinogun/deployer/internal/reconcile"
	"github.com/toyinogun/deployer/internal/store"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// namespace is where this world's app lands. The slug carries a random suffix,
// so it is read off the app rather than spelled out.
func (w *world) namespace() string { return deploy.NamespaceName(w.app.Slug) }

func TestADeployFencesTheNamespace(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	ctx := t.Context()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(w.deployment))

	api := w.clientset.NetworkingV1().NetworkPolicies(w.namespace())
	deny, err := api.Get(ctx, deploy.DenyPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading %s back: %v", deploy.DenyPolicyName, err)
	}
	if len(deny.Spec.Ingress) != 0 || len(deny.Spec.Egress) != 0 {
		t.Errorf("%s carries rules, so it is not a deny", deploy.DenyPolicyName)
	}
	allow, err := api.Get(ctx, deploy.AllowPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading %s back: %v", deploy.AllowPolicyName, err)
	}
	// The configured list reaches the composed object, rather than an empty
	// except silently turning the allow rule into an open door.
	except := allow.Spec.Egress[1].To[0].IPBlock.Except
	if len(except) != 1 || except[0] != "10.0.0.0/8" {
		t.Errorf("except = %v, want the configured [10.0.0.0/8]", except)
	}
}

// The order is the whole point: a Deployment that lands before its policies is
// an app that ran unpoliced, however briefly (AC-13).
func TestThePoliciesAreWrittenBeforeTheDeployment(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	ctx := t.Context()

	var order []string
	w.clientset.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		switch action.GetResource().Resource {
		case "networkpolicies", "deployments":
			order = append(order, action.GetResource().Resource)
		}
		return false, nil, nil
	})

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(w.deployment))

	if len(order) < 3 {
		t.Fatalf("writes = %v, want both policies and the deployment", order)
	}
	if order[0] != "networkpolicies" || order[1] != "networkpolicies" || order[2] != "deployments" {
		t.Errorf("write order = %v, want both networkpolicies before deployments", order)
	}
}

func TestAPolicyWriteFailureEndsTheDeploymentWithNoWorkload(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	// No appComesUp here: nothing should get as far as reading a Deployment back,
	// and a stubbed read would hide exactly the thing this asserts.
	ctx := t.Context()

	w.clientset.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server said no")
	})

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(w.deployment))

	dep, err := w.store.GetDeployment(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if dep.State != string(domain.StateFailed) {
		t.Fatalf("state = %s, want failed", dep.State)
	}
	if dep.FailureReason == nil || *dep.FailureReason != string(domain.ReasonInternal) {
		t.Errorf("failure reason = %v, want %s", dep.FailureReason, domain.ReasonInternal)
	}
	// Nothing is running, which is the half of AC-13 that actually protects the
	// cluster: an unfenced app must not exist at all.
	_, err = w.clientset.AppsV1().Deployments(w.namespace()).Get(ctx, deploy.WorkloadName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("the app Deployment exists after the fence failed: %v", err)
	}
}

// A policy someone deleted or weakened by hand comes back on the next deploy,
// identical, because its content is code and configuration rather than anything
// a caller sent (AC-11).
func TestARedeployRestoresAWeakenedPolicy(t *testing.T) {
	w := setup(t)
	w.buildEnds(batchv1.JobComplete)
	w.appComesUp()
	ctx := t.Context()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, toLoop(w.deployment))

	api := w.clientset.NetworkingV1().NetworkPolicies(w.namespace())
	weakened, err := api.Get(ctx, deploy.AllowPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading %s back: %v", deploy.AllowPolicyName, err)
	}
	weakened.Spec.Egress[1].To[0].IPBlock.Except = nil
	if _, err := api.Update(ctx, weakened, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("weakening the policy by hand: %v", err)
	}

	second := w.queueAnother(t)
	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).Drive(ctx, second)

	restored, err := api.Get(ctx, deploy.AllowPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading %s back after the redeploy: %v", deploy.AllowPolicyName, err)
	}
	if except := restored.Spec.Egress[1].To[0].IPBlock.Except; len(except) != 1 {
		t.Errorf("except = %v after a redeploy, want the configured list back", except)
	}
}

// PolicySweep polices what is on the cluster, so a namespace from before this
// slice is fenced without being redeployed (AC-12).
func TestPolicySweepFencesExistingNamespaces(t *testing.T) {
	w := setup(t)
	ctx := t.Context()
	w.clientset = fake.NewClientset(
		appNamespaceObject("app-old", "old"),
		appNamespaceObject("app-older", "older"),
		// Labelled by ArgoCD rather than by the platform, so the selector must
		// step over it: the control plane's own namespace is not an app.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "deployer-system",
			Labels: map[string]string{"app.kubernetes.io/managed-by": "argocd"},
		}},
	)

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).PolicySweep(ctx)

	for _, ns := range []string{"app-old", "app-older"} {
		for _, name := range []string{deploy.DenyPolicyName, deploy.AllowPolicyName} {
			if _, err := w.clientset.NetworkingV1().NetworkPolicies(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
				t.Errorf("%s/%s missing after the sweep: %v", ns, name, err)
			}
		}
	}
	list, err := w.clientset.NetworkingV1().NetworkPolicies("deployer-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing policies in deployer-system: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("the sweep policed deployer-system, which is not an app namespace")
	}
}

// One namespace that will not take its policies must not leave the rest open.
func TestPolicySweepCarriesOnPastAFailure(t *testing.T) {
	w := setup(t)
	ctx := t.Context()
	w.clientset = fake.NewClientset(
		appNamespaceObject("app-broken", "broken"),
		appNamespaceObject("app-fine", "fine"),
	)
	w.clientset.PrependReactor("create", "networkpolicies", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "app-broken" {
			return true, nil, errors.New("the API server said no")
		}
		return false, nil, nil
	})

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).PolicySweep(ctx)

	if _, err := w.clientset.NetworkingV1().NetworkPolicies("app-fine").Get(ctx, deploy.AllowPolicyName, metav1.GetOptions{}); err != nil {
		t.Errorf("app-fine was left unpoliced by a failure in another namespace: %v", err)
	}
}

// The sweep touches no deployment state at all, which is what keeps it a
// different thing from Sweep despite the shared word.
func TestPolicySweepWritesNoDeploymentState(t *testing.T) {
	w := setup(t)
	ctx := t.Context()

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}).PolicySweep(ctx)

	dep, err := w.store.GetDeployment(ctx, w.deployment.ID)
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if dep.State != string(domain.StateQueued) {
		t.Errorf("state = %s after a policy sweep, want it untouched at queued", dep.State)
	}
}

func TestAppNamespacesReadsTheSlugLabel(t *testing.T) {
	ctx := t.Context()
	cs := fake.NewClientset(
		appNamespaceObject("app-one", "one"),
		// Managed by the platform but carrying no slug: not something to compose
		// a policy for blind.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "app-nameless",
			Labels: map[string]string{"app.kubernetes.io/managed-by": "deployer"},
		}},
	)

	slugs, err := kube.NewFor(cs).AppNamespaces(ctx)
	if err != nil {
		t.Fatalf("listing app namespaces: %v", err)
	}
	if len(slugs) != 1 || slugs[0] != "one" {
		t.Errorf("slugs = %v, want [one]", slugs)
	}
}

// appNamespaceObject is an app namespace as the platform composes one.
func appNamespaceObject(name, slug string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "deployer",
			"deployer.internal/app-slug":   slug,
		},
	}}
}

// queueAnother uploads again and queues a second deployment for the same app,
// which is exactly what a redeploy is.
func (w *world) queueAnother(t *testing.T) reconcile.Deployment {
	t.Helper()
	ctx := t.Context()
	up, err := w.uploads.Accept(ctx, w.accountID, strings.NewReader("\x1f\x8b"+strings.Repeat("y", 64)))
	if err != nil {
		t.Fatalf("accepting the second upload: %v", err)
	}
	dep, _, err := w.store.CreateDeployment(ctx, store.CreateDeploymentInput{
		AppID: w.app.ID, AccountID: w.accountID, UploadID: &up.ID,
	})
	if err != nil {
		t.Fatalf("queueing the redeploy: %v", err)
	}
	return toLoop(dep)
}
