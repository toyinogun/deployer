package kube

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
