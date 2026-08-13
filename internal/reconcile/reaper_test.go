package reconcile_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/kube"
	"github.com/toyinogun/deployer/internal/reconcile"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// agedNamespace is an app namespace as the platform composes one, with its
// creation stamped explicitly. The fake clientset leaves CreationTimestamp zero
// unless it is set, and a zero timestamp is older than every grace, so a test
// that skipped this would pass for the wrong reason (spec 0012, critical test
// scenarios).
func agedNamespace(name, slug string, created time.Time) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "deployer",
			"deployer.internal/app-slug":   slug,
		},
		CreationTimestamp: metav1.NewTime(created),
	}}
}

// namespaceNames is what is left on the cluster after a pass.
func namespaceNames(t *testing.T, cs *fake.Clientset) []string {
	t.Helper()
	list, err := cs.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing namespaces: %v", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, ns := range list.Items {
		names = append(names, ns.Name)
	}
	return names
}

// withGrace sets the reaper's two knobs on the harness reconciler.
func withGrace(grace time.Duration) func(*reconcile.Options) {
	return func(o *reconcile.Options) {
		o.OrphanGrace = grace
		o.ReapInterval = time.Minute
	}
}

// TestTheReaperDeletesOnlyNamespacesNoLiveAppOwns is the whole point of the
// loop: the app the store still has stays, and the one nothing owns goes.
func TestTheReaperDeletesOnlyNamespacesNoLiveAppOwns(t *testing.T) {
	// covers: AC-23, AC-25
	w := setup(t)
	now := time.Now().UTC()
	w.clientset = fake.NewClientset(
		// The harness app's own namespace, which has a live row behind it.
		agedNamespace("app-"+w.app.Slug, w.app.Slug, now.Add(-time.Hour)),
		agedNamespace("app-orphan-a1b2c3", "orphan-a1b2c3", now.Add(-time.Hour)),
		// Managed by the platform but carrying no slug label, so it is invisible
		// to the selector and never reaped, which is the safe direction.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:              "app-nameless",
			Labels:            map[string]string{"app.kubernetes.io/managed-by": "deployer"},
			CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
		}},
		// Not the platform's at all.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:              "app-someone-elses",
			CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
		}},
	)

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, withGrace(time.Minute)).
		ReapOrphanNamespaces(t.Context(), now)

	left := namespaceNames(t, w.clientset)
	for _, want := range []string{"app-" + w.app.Slug, "app-nameless", "app-someone-elses"} {
		if !contains(left, want) {
			t.Errorf("%s was reaped, want it left alone; left = %v", want, left)
		}
	}
	if contains(left, "app-orphan-a1b2c3") {
		t.Errorf("the orphan namespace survived the pass; left = %v", left)
	}
}

// TestTheGraceKeepsAFreshNamespaceOutOfReach pins the boundary in both
// directions, because the window it guards is a namespace a deploy created
// seconds ago.
func TestTheGraceKeepsAFreshNamespaceOutOfReach(t *testing.T) {
	// covers: AC-26
	for _, tc := range []struct {
		name   string
		age    time.Duration
		reaped bool
	}{
		{name: "younger than the grace", age: 5 * time.Minute, reaped: false},
		{name: "older than the grace", age: 20 * time.Minute, reaped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := setup(t)
			now := time.Now().UTC()
			w.clientset = fake.NewClientset(agedNamespace("app-orphan-a1b2c3", "orphan-a1b2c3", now.Add(-tc.age)))

			w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, withGrace(15*time.Minute)).
				ReapOrphanNamespaces(t.Context(), now)

			gone := !contains(namespaceNames(t, w.clientset), "app-orphan-a1b2c3")
			if gone != tc.reaped {
				t.Errorf("reaped = %v at an age of %s, want %v", gone, tc.age, tc.reaped)
			}
		})
	}
}

// TestAFailedSlugReadReapsNothing is the guard that makes this loop safe to run
// unattended: a database that cannot answer must never read as "no app owns
// this".
func TestAFailedSlugReadReapsNothing(t *testing.T) {
	// covers: AC-24
	w := setup(t)
	now := time.Now().UTC()
	w.clientset = fake.NewClientset(agedNamespace("app-orphan-a1b2c3", "orphan-a1b2c3", now.Add(-time.Hour)))
	deleted := false
	w.clientset.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return false, nil, nil
	})

	loop := reconcile.New(nil, brokenSlugs{}, uploadsFor{w.uploads},
		fakeRegistry{digest: testDigest, user: "1000"}, kube.NewFor(w.clientset),
		reconcile.Options{OrphanGrace: time.Minute, ReapInterval: time.Minute})
	loop.ReapOrphanNamespaces(t.Context(), now)

	if deleted {
		t.Error("the pass deleted a namespace after the live slug read failed")
	}
}

// TestOneUndeletableNamespaceDoesNotStopThePass keeps a single stuck namespace
// from leaving every other orphan behind.
func TestOneUndeletableNamespaceDoesNotStopThePass(t *testing.T) {
	// covers: AC-27
	w := setup(t)
	now := time.Now().UTC()
	w.clientset = fake.NewClientset(
		agedNamespace("app-broken-a1b2c3", "broken-a1b2c3", now.Add(-time.Hour)),
		agedNamespace("app-fine-a1b2c3", "fine-a1b2c3", now.Add(-time.Hour)),
	)
	w.clientset.PrependReactor("delete", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.DeleteAction).GetName() == "app-broken-a1b2c3" {
			return true, nil, errors.New("the API server said no")
		}
		return false, nil, nil
	})

	w.reconciler(fakeRegistry{digest: testDigest, user: "1000"}, withGrace(time.Minute)).
		ReapOrphanNamespaces(t.Context(), now)

	if contains(namespaceNames(t, w.clientset), "app-fine-a1b2c3") {
		t.Error("a failure on one namespace left another orphan behind")
	}
}

// brokenSlugs is a store whose live slug read fails. Every other method panics,
// because the pass must never reach one.
type brokenSlugs struct{ reconcile.Apps }

func (brokenSlugs) LiveAppSlugs(context.Context) ([]string, error) {
	return nil, errors.New("the database is unreachable")
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
