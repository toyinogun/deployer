package kube

import (
	"errors"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/deploy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// appNamespace is an app namespace as internal/deploy composes one, with its
// creation stamped: the fake clientset leaves that field zero unless it is set,
// and a zero timestamp is older than every grace.
func appNamespace(slug string, created time.Time, phase corev1.NamespacePhase) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: deploy.NamespaceName(slug),
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "deployer",
				"deployer.internal/app-slug":   slug,
			},
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.NamespaceStatus{Phase: phase},
	}
}

// TestDeleteNamespaceTreatsGoneAndTerminatingAsDone keeps that knowledge in this
// package, so no handler above it ever inspects a Kubernetes error.
func TestDeleteNamespaceTreatsGoneAndTerminatingAsDone(t *testing.T) {
	// covers: AC-18
	now := time.Now()
	for _, tc := range []struct {
		name    string
		objects []runtime.Object
	}{
		{name: "never deployed, so no namespace exists"},
		{name: "already terminating", objects: []runtime.Object{
			appNamespace("demo", now, corev1.NamespaceTerminating),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewFor(fake.NewSimpleClientset(tc.objects...))
			if err := c.DeleteNamespace(t.Context(), "demo"); err != nil {
				t.Errorf("DeleteNamespace = %v, want success", err)
			}
		})
	}
}

// TestDeleteNamespaceRemovesALiveNamespace is the ordinary path: one call, and
// Kubernetes cascades everything inside.
func TestDeleteNamespaceRemovesALiveNamespace(t *testing.T) {
	// covers: AC-16
	cs := fake.NewSimpleClientset(appNamespace("demo", time.Now(), corev1.NamespaceActive))
	if err := NewFor(cs).DeleteNamespace(t.Context(), "demo"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(t.Context(), deploy.NamespaceName("demo"), metav1.GetOptions{}); err == nil {
		t.Error("the namespace is still there after the delete")
	}
}

// TestDeleteNamespaceReportsAnyOtherFailure pins that only the two tolerated
// cases are tolerated: everything else is a wrapped error for the caller to
// refuse on.
func TestDeleteNamespaceReportsAnyOtherFailure(t *testing.T) {
	// covers: AC-19
	cs := fake.NewSimpleClientset(appNamespace("demo", time.Now(), corev1.NamespaceActive))
	cs.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server said no")
	})
	if err := NewFor(cs).DeleteNamespace(t.Context(), "demo"); err == nil {
		t.Error("DeleteNamespace reported success after the API server refused it")
	}
}

// TestAppNamespacesOlderThanAgesAgainstThePassedNow pins the grace boundary to
// the second, which is only possible because now is a parameter here.
func TestAppNamespacesOlderThanAgesAgainstThePassedNow(t *testing.T) {
	// covers: AC-25, AC-26
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	cs := fake.NewSimpleClientset(
		appNamespace("old", now.Add(-16*time.Minute), corev1.NamespaceActive),
		appNamespace("fresh", now.Add(-14*time.Minute), corev1.NamespaceActive),
		// Managed by the platform but with no slug label, so it is invisible here
		// and can never be reaped.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:              "app-nameless",
			Labels:            map[string]string{"app.kubernetes.io/managed-by": "deployer"},
			CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
		}},
		// Not the platform's, so outside the selector entirely.
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:              "app-someone-elses",
			CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
		}},
	)

	slugs, err := NewFor(cs).AppNamespacesOlderThan(t.Context(), now, 15*time.Minute)
	if err != nil {
		t.Fatalf("AppNamespacesOlderThan: %v", err)
	}
	if len(slugs) != 1 || slugs[0] != "old" {
		t.Errorf("slugs = %v, want [old]: only a namespace past the grace, carrying both labels", slugs)
	}
}
