package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/deploy"
	"github.com/toyinogun/deployer/internal/logs"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// pod builds one app pod the way deploy labels them, so the selector under test
// is the real one rather than a copy that could drift.
func pod(name, slug string, ageMinutes int, ready, started bool, restarts int32) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         deploy.NamespaceName(slug),
			Labels:            deploy.Selector(slug),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Duration(ageMinutes) * time.Minute)),
		},
	}
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}}
	cs := corev1.ContainerStatus{Name: deploy.WorkloadName, RestartCount: restarts}
	if !started {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}
	} else {
		cs.State.Running = &corev1.ContainerStateRunning{}
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	return p
}

func TestPodsForAppReturnsNewestFirst(t *testing.T) {
	c := NewFor(fake.NewSimpleClientset(
		pod("old", "demo", 30, true, true, 0),
		pod("new", "demo", 1, false, true, 3),
	))

	got, err := c.PodsForApp(context.Background(), deploy.NamespaceName("demo"), "demo")
	if err != nil {
		t.Fatalf("PodsForApp: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d pods, want 2", len(got))
	}
	if got[0].Name != "new" {
		t.Fatalf("newest first is not honoured: %+v", got)
	}
	if got[0].Ready || got[0].RestartCount != 3 || !got[0].ContainerStarted {
		t.Fatalf("the newest pod's status is wrong: %+v", got[0])
	}
	if !got[1].Ready {
		t.Fatalf("the older pod should read ready: %+v", got[1])
	}
}

func TestPodsForAppIsScopedToTheApp(t *testing.T) {
	// Another app's pod in another namespace must be invisible, which is the
	// whole of AC-11 at this layer.
	c := NewFor(fake.NewSimpleClientset(
		pod("mine", "demo", 1, true, true, 0),
		pod("theirs", "other", 1, true, true, 0),
	))

	got, err := c.PodsForApp(context.Background(), deploy.NamespaceName("demo"), "demo")
	if err != nil {
		t.Fatalf("PodsForApp: %v", err)
	}
	if len(got) != 1 || got[0].Name != "mine" {
		t.Fatalf("the read escaped the app's namespace: %+v", got)
	}
}

func TestPodsForAppWithNoPods(t *testing.T) {
	c := NewFor(fake.NewSimpleClientset())
	got, err := c.PodsForApp(context.Background(), deploy.NamespaceName("demo"), "demo")
	if err != nil {
		t.Fatalf("no pods should not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d pods, want 0", len(got))
	}
}

func TestPodStatusWithNoContainerStatuses(t *testing.T) {
	// A pod the scheduler has accepted but whose container has no status yet:
	// not started, and never an index into position zero.
	p := pod("pending", "demo", 1, false, false, 0)
	p.Status.ContainerStatuses = nil
	c := NewFor(fake.NewSimpleClientset(p))

	got, err := c.PodsForApp(context.Background(), deploy.NamespaceName("demo"), "demo")
	if err != nil {
		t.Fatalf("PodsForApp: %v", err)
	}
	if len(got) != 1 || got[0].ContainerStarted {
		t.Fatalf("an empty container status list read as started: %+v", got)
	}
}

func TestPodStatusWithAWaitingContainer(t *testing.T) {
	c := NewFor(fake.NewSimpleClientset(pod("waiting", "demo", 1, false, false, 0)))
	got, err := c.PodsForApp(context.Background(), deploy.NamespaceName("demo"), "demo")
	if err != nil {
		t.Fatalf("PodsForApp: %v", err)
	}
	if got[0].ContainerStarted {
		t.Fatalf("a Waiting container read as started: %+v", got[0])
	}
}

func TestPodLogReads(t *testing.T) {
	// The fake clientset serves a canned body, which is enough to prove the
	// request is made and the response is returned whole. What the lines mean is
	// internal/logs' business.
	c := NewFor(fake.NewSimpleClientset(pod("new", "demo", 1, true, true, 1)))
	raw, err := c.PodLog(context.Background(), deploy.NamespaceName("demo"), "new", 200, false)
	if err != nil {
		t.Fatalf("PodLog: %v", err)
	}
	if raw == "" {
		t.Fatal("PodLog returned nothing")
	}
	if _, err := c.PodLog(context.Background(), deploy.NamespaceName("demo"), "new", 100, true); err != nil {
		t.Fatalf("PodLog of the previous container: %v", err)
	}
}

// The namespace an app's pods live in is created at the deploy step, which runs
// after the build finishes. A log read during the build therefore lists pods in
// a namespace the control plane holds no RoleBinding in, and Kubernetes answers
// Forbidden whether or not the namespace exists. That is the app's container not
// having started, not a fault (spec 0006, AC-7).
func TestPodsForAppWhileTheNamespaceIsNotThereYet(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forbidden", apierrors.NewForbidden(corev1.Resource("pods"), "", errors.New("no binding yet"))},
		{"not found", apierrors.NewNotFound(corev1.Resource("namespaces"), "app-demo")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			got, err := NewFor(cs).PodsForApp(context.Background(), deploy.NamespaceName("demo"), "demo")
			if !errors.Is(err, logs.ErrNoNamespace) {
				t.Fatalf("got error %v, want one wrapping logs.ErrNoNamespace", err)
			}
			if got != nil {
				t.Fatalf("got %d pods, want none", len(got))
			}
		})
	}
}

// workload builds a Deployment whose status says what the rollout is doing, so
// the readiness check under test reads the same counts Kubernetes reports.
func workload(generation, observed, replicas, updated, available int32) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       deploy.WorkloadName,
			Namespace:  deploy.NamespaceName("demo"),
			Generation: int64(generation),
		},
		Spec: appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: int64(observed),
			Replicas:           replicas,
			UpdatedReplicas:    updated,
			AvailableReplicas:  available,
		},
	}
}

// A rolling update keeps the old pod serving while the new one starts, so the
// old pod's availability must never read as the new one's. Rolling back to a
// release whose image cannot be pulled reached healthy in milliseconds on the
// real cluster because of exactly this, and the failed rollback was recorded as
// the app's current release (spec 0011, AC-17).
func TestWorkloadReady(t *testing.T) {
	cases := []struct {
		name                                        string
		generation, observed, replicas, updated, av int32
		want                                        bool
	}{
		{"a finished rollout is ready", 2, 2, 1, 1, 1, true},
		{"the new pod exists but the old one supplies the available replica", 2, 2, 2, 1, 1, false},
		{"the new pod is up and the old one is still terminating", 2, 2, 2, 1, 2, false},
		{"the new pod exists and nothing is available", 2, 2, 1, 1, 0, false},
		{"the controller has not seen this spec yet", 3, 2, 1, 1, 1, false},
		{"no new pod has been created", 2, 2, 1, 0, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewFor(fake.NewSimpleClientset(workload(tc.generation, tc.observed, tc.replicas, tc.updated, tc.av)))
			got, err := c.WorkloadReady(context.Background(), deploy.NamespaceName("demo"), deploy.WorkloadName)
			if err != nil {
				t.Fatalf("WorkloadReady: %v", err)
			}
			if got != tc.want {
				t.Errorf("WorkloadReady = %v, want %v", got, tc.want)
			}
		})
	}
}

// A Deployment that is not there yet is not a fault: the deploy step creates it,
// and the wait starts before the controller has caught up.
func TestWorkloadReadyMissingDeploymentIsNotAnError(t *testing.T) {
	c := NewFor(fake.NewSimpleClientset())
	got, err := c.WorkloadReady(context.Background(), deploy.NamespaceName("demo"), deploy.WorkloadName)
	if err != nil {
		t.Fatalf("WorkloadReady: %v", err)
	}
	if got {
		t.Error("WorkloadReady = true for a Deployment that does not exist")
	}
}
