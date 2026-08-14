package kube

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/toyinogun/deployer/internal/deploy"
)

// scalable is a Deployment at the given replica count, in the shape the deploy
// path composes.
func scalable(namespace string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: deploy.WorkloadName},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

// TestScaleWorkloadStopsAndStartsAWorkload is both directions of the one write a
// suspension makes. It touches the replica count and nothing else, which is what
// lets a restore bring an app back on the exact image it was serving with no
// rebuild. covers: AC-3, AC-4
func TestScaleWorkloadStopsAndStartsAWorkload(t *testing.T) {
	t.Parallel()
	ns := deploy.NamespaceName("live")
	c := NewFor(fake.NewSimpleClientset(scalable(ns, 1)))

	if err := c.ScaleWorkload(t.Context(), ns, deploy.WorkloadName, 0); err != nil {
		t.Fatalf("scaling to zero: %v", err)
	}
	if got := replicasOf(t, c, ns); got != 0 {
		t.Errorf("after a suspension the Deployment is at %d replicas, want 0", got)
	}

	if err := c.ScaleWorkload(t.Context(), ns, deploy.WorkloadName, deploy.ServingReplicas); err != nil {
		t.Fatalf("scaling back: %v", err)
	}
	if got := replicasOf(t, c, ns); got != deploy.ServingReplicas {
		t.Errorf("after a restore the Deployment is at %d replicas, want %d", got, deploy.ServingReplicas)
	}
}

// TestScaleWorkloadTreatsAMissingDeploymentAsSuccess is AC-5. A namespace that is
// gone, or an app that never reached the cluster, is not a fault of the caller
// asking for zero pods, and the sweep would otherwise log a failure every tick
// for every app that was cleaned up. covers: AC-5
func TestScaleWorkloadTreatsAMissingDeploymentAsSuccess(t *testing.T) {
	t.Parallel()
	c := NewFor(fake.NewSimpleClientset())

	if err := c.ScaleWorkload(t.Context(), deploy.NamespaceName("nowhere"), deploy.WorkloadName, 0); err != nil {
		t.Errorf("scaling a Deployment that is not there returned %v, want success", err)
	}
	if err := c.ScaleWorkload(t.Context(), deploy.NamespaceName("nowhere"), deploy.WorkloadName,
		deploy.ServingReplicas); err != nil {
		t.Errorf("restoring a Deployment that is not there returned %v, want success", err)
	}
}

// TestScaleWorkloadTreatsAVanishedNamespaceAsSuccess is the shape AC-5 actually
// takes on a real cluster, which the fake clientset cannot produce by itself.
//
// The rights to read a Deployment come from a RoleBinding the control plane
// creates inside the app's own namespace, deliberately, so its reach is exactly
// the namespaces that exist (deploy/rbac.yaml, ClusterRole/deployer-app). Delete
// the namespace and the binding goes with it, so the API answers forbidden, not
// not found. `/check verify` found the sweep logging an error for nine such apps
// every tick, thirty seven a minute, while the suspend response named them all
// as apps that did not stop. covers: AC-5, AC-6
func TestScaleWorkloadTreatsAVanishedNamespaceAsSuccess(t *testing.T) {
	t.Parallel()
	ns := deploy.NamespaceName("gone")
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, deploy.WorkloadName,
			errors.New("no RoleBinding in a namespace that is not there"))
	})
	c := NewFor(cs)

	if err := c.ScaleWorkload(t.Context(), ns, deploy.WorkloadName, 0); err != nil {
		t.Errorf("scaling an app whose namespace is gone returned %v, want success", err)
	}
	if err := c.ScaleWorkload(t.Context(), ns, deploy.WorkloadName, deploy.ServingReplicas); err != nil {
		t.Errorf("restoring an app whose namespace is gone returned %v, want success", err)
	}
}

// TestScaleWorkloadKeepsARealRefusalAnError is the other half, and the reason the
// fix is not simply to swallow forbidden.
//
// Suspension is an availability control: an app the platform failed to stop must
// be reported, never quietly counted as stopped. So forbidden is only success
// when the namespace really is gone. Here it is present, which means the binding
// is broken, and that is a fault the admin has to see. covers: AC-5, AC-6
func TestScaleWorkloadKeepsARealRefusalAnError(t *testing.T) {
	t.Parallel()
	ns := deploy.NamespaceName("live")
	cs := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	cs.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, deploy.WorkloadName,
			errors.New("the binding is broken"))
	})
	c := NewFor(cs)

	if err := c.ScaleWorkload(t.Context(), ns, deploy.WorkloadName, 0); err == nil {
		t.Error("a refusal inside a namespace that exists was reported as success, so an app nobody stopped reads as stopped")
	}
}

// TestScaleWorkloadIsIdempotent is why the sweep can run forever: writing a count
// a Deployment already carries changes nothing and accumulates nothing.
// covers: AC-7
func TestScaleWorkloadIsIdempotent(t *testing.T) {
	t.Parallel()
	ns := deploy.NamespaceName("live")
	c := NewFor(fake.NewSimpleClientset(scalable(ns, 0)))

	for range 3 {
		if err := c.ScaleWorkload(t.Context(), ns, deploy.WorkloadName, 0); err != nil {
			t.Fatalf("scaling an already stopped Deployment: %v", err)
		}
	}
	if got := replicasOf(t, c, ns); got != 0 {
		t.Errorf("the Deployment is at %d replicas, want 0", got)
	}
}

// replicasOf reads the count back off the cluster rather than off what the caller
// asked for.
func replicasOf(t *testing.T, c *Client, namespace string) int32 {
	t.Helper()
	d, err := c.cs.AppsV1().Deployments(namespace).Get(t.Context(), deploy.WorkloadName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the deployment back: %v", err)
	}
	if d.Spec.Replicas == nil {
		t.Fatal("the deployment carries no replica count at all")
	}
	return *d.Spec.Replicas
}
