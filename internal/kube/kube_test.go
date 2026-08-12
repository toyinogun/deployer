package kube

import (
	"context"
	"testing"
	"time"

	"github.com/toyinogun/deployer/internal/deploy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
