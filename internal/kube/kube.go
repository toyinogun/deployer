// Package kube is the platform's only door to the Kubernetes API. It holds the
// client and nothing else: what to create is decided in internal/deploy and
// internal/build, which compose objects without ever seeing a client.
//
// Everything here is create or update, never delete and recreate, so a deploy
// that fails partway leaves what it made and the next one reconciles it
// (spec 0004, Key invariants).
package kube

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/toyinogun/deployer/internal/build"
	"github.com/toyinogun/deployer/internal/deploy"
	"github.com/toyinogun/deployer/internal/logs"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Client wraps a Kubernetes clientset.
type Client struct{ cs kubernetes.Interface }

// New returns a client authenticated as the pod's own service account.
func New() (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kube: reading the in cluster configuration: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building a client: %w", err)
	}
	return &Client{cs: cs}, nil
}

// NewFor returns a client over an existing clientset, which is what the tests
// hand the fake one.
func NewFor(cs kubernetes.Interface) *Client { return &Client{cs: cs} }

// EnsureNamespace creates an app's namespace and the objects that fence it, and
// leaves every one of them alone if it already exists.
//
// Left alone is the point: the quota, the limits, and the pod security labels
// are the fence around an app, and a fence the platform rewrites on every deploy
// is not a fence (spec 0004, AC-11).
func (c *Client) EnsureNamespace(ctx context.Context, ns *corev1.Namespace, rb *rbacv1.RoleBinding, quota *corev1.ResourceQuota, limits *corev1.LimitRange) error {
	if _, err := c.cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); ignoreExists(err) != nil {
		return fmt.Errorf("kube: creating namespace %s: %w", ns.Name, err)
	}
	if _, err := c.cs.RbacV1().RoleBindings(rb.Namespace).Create(ctx, rb, metav1.CreateOptions{}); ignoreExists(err) != nil {
		return fmt.Errorf("kube: creating the role binding in %s: %w", rb.Namespace, err)
	}
	if _, err := c.cs.CoreV1().ResourceQuotas(quota.Namespace).Create(ctx, quota, metav1.CreateOptions{}); ignoreExists(err) != nil {
		return fmt.Errorf("kube: creating the quota in %s: %w", quota.Namespace, err)
	}
	if _, err := c.cs.CoreV1().LimitRanges(limits.Namespace).Create(ctx, limits, metav1.CreateOptions{}); ignoreExists(err) != nil {
		return fmt.Errorf("kube: creating the limit range in %s: %w", limits.Namespace, err)
	}
	return nil
}

// ApplyNetworkPolicies writes an app's fence over whatever is already there.
//
// Applied rather than created if absent, which is the one place this package
// departs from the leave it alone rule above, and it departs on purpose: the
// quota and the limits were left alone so the platform could not move a fence
// someone had tightened, and these are rewritten so a fence someone loosened
// comes back on the next deploy. Overwriting can only restore the platform's own
// policy, because none of its content is caller derived (spec 0008, AC-11).
func (c *Client) ApplyNetworkPolicies(ctx context.Context, policies ...*networkingv1.NetworkPolicy) error {
	for _, p := range policies {
		api := c.cs.NetworkingV1().NetworkPolicies(p.Namespace)
		_, err := api.Create(ctx, p, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			var current *networkingv1.NetworkPolicy
			current, err = api.Get(ctx, p.Name, metav1.GetOptions{})
			if err == nil {
				current.Labels = p.Labels
				current.Spec = p.Spec
				_, err = api.Update(ctx, current, metav1.UpdateOptions{})
			}
		}
		if err != nil {
			return fmt.Errorf("kube: writing network policy %s/%s: %w", p.Namespace, p.Name, err)
		}
	}
	return nil
}

// AppNamespaces lists the slug of every app namespace the platform owns, read
// off the cluster rather than the database: the startup sweep polices what is
// actually there, including a namespace whose row was since removed
// (spec 0008, AC-12).
func (c *Client) AppNamespaces(ctx context.Context) ([]string, error) {
	list, err := c.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: managedByDeployer,
	})
	if err != nil {
		return nil, fmt.Errorf("kube: listing app namespaces: %w", err)
	}
	slugs := make([]string, 0, len(list.Items))
	for _, ns := range list.Items {
		// The control plane's own namespace and the build namespace are labelled
		// by ArgoCD, not by the platform, so this selector already excludes them.
		// The slug label is what makes a namespace one this package composes for;
		// anything without it is not something to police blind.
		if slug := ns.Labels[appSlugLabel]; slug != "" {
			slugs = append(slugs, slug)
		}
	}
	return slugs, nil
}

// The labels an app namespace carries, set in internal/deploy when it is created.
const (
	managedByDeployer = "app.kubernetes.io/managed-by=deployer"
	appSlugLabel      = "deployer.internal/app-slug"
)

// ApplySecret creates or updates one secret. The pull secret is refreshed on
// every deploy through this, so a rotated registry credential reaches an app
// namespace without anything being deleted.
func (c *Client) ApplySecret(ctx context.Context, s *corev1.Secret) error {
	api := c.cs.CoreV1().Secrets(s.Namespace)
	// Read before writing, rather than creating and falling back on
	// AlreadyExists. A ResourceQuota is charged when a create reaches admission,
	// and the charge is not returned when the create then fails, so a redeploy
	// that creates what is already there walks the namespace quota upward until
	// Kubernetes recalculates it. Rollbacks are cheap enough to run several in a
	// row, which is how an app with a quota of 3 services started refusing them.
	_, err := api.Get(ctx, s.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = api.Create(ctx, s, metav1.CreateOptions{})
	case err == nil:
		_, err = api.Update(ctx, s, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("kube: writing secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return nil
}

// CreateJob creates a build Job and returns an owner reference to it, which is
// what the per Job credential secret hangs off so Kubernetes collects the
// credential when the Job is reaped.
//
// An existing Job is adopted rather than refused: after a restart the sweep runs
// against a build that may already be underway, and one Job is one attempt.
func (c *Client) CreateJob(ctx context.Context, job *batchv1.Job) (metav1.OwnerReference, error) {
	api := c.cs.BatchV1().Jobs(job.Namespace)
	created, err := api.Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		created, err = api.Get(ctx, job.Name, metav1.GetOptions{})
	}
	if err != nil {
		return metav1.OwnerReference{}, fmt.Errorf("kube: creating job %s/%s: %w", job.Namespace, job.Name, err)
	}
	return metav1.OwnerReference{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       created.Name,
		UID:        created.UID,
	}, nil
}

// DeleteJob removes a build Job and, through background propagation, the pod it
// made. It is the one delete in this package: a deployment the watchdog gives up
// on must not leave a build running against a row nothing will read
// (spec 0005, AC-15).
//
// A Job that is already gone is success, because the only thing asked for was
// that it not be running.
func (c *Client) DeleteJob(ctx context.Context, namespace, name string) error {
	policy := metav1.DeletePropagationBackground
	err := c.cs.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kube: deleting job %s/%s: %w", namespace, name, err)
	}
	return nil
}

// JobState reports where a build Job has got to.
func (c *Client) JobState(ctx context.Context, namespace, name string) (build.JobState, error) {
	job, err := c.cs.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return build.JobGone, nil
	}
	if err != nil {
		return build.JobRunning, fmt.Errorf("kube: reading job %s/%s: %w", namespace, name, err)
	}
	for _, cond := range job.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			return build.JobSucceeded, nil
		case batchv1.JobFailed:
			// Includes the deadline being exceeded, which is a failed build
			// rather than a separate outcome: either way no image landed.
			return build.JobFailed, nil
		}
	}
	return build.JobRunning, nil
}

// ApplyWorkload creates or updates an app's Deployment, Service, and Ingress.
//
// Update in place, never delete and recreate, so a redeploy is a rolling change
// on a live app rather than a gap in service (AC-13).
func (c *Client) ApplyWorkload(ctx context.Context, d *appsv1.Deployment, s *corev1.Service, i *networkingv1.Ingress) error {
	deployments := c.cs.AppsV1().Deployments(d.Namespace)
	if _, err := deployments.Create(ctx, d, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		if err := c.updateDeployment(ctx, d); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("kube: creating deployment %s/%s: %w", d.Namespace, d.Name, err)
	}

	// Read before writing, for the quota reason ApplySecret carries: a Service is
	// quota tracked, and a create that fails with AlreadyExists has already been
	// charged for.
	services := c.cs.CoreV1().Services(s.Namespace)
	current, err := services.Get(ctx, s.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := services.Create(ctx, s, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("kube: creating service %s/%s: %w", s.Namespace, s.Name, err)
		}
	case err != nil:
		return fmt.Errorf("kube: reading service %s/%s: %w", s.Namespace, s.Name, err)
	default:
		// A Service holds a cluster IP the API server assigned, so the existing
		// spec is edited rather than replaced wholesale.
		current.Spec.Selector = s.Spec.Selector
		current.Spec.Ports = s.Spec.Ports
		if _, err := services.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("kube: updating service %s/%s: %w", s.Namespace, s.Name, err)
		}
	}

	ingresses := c.cs.NetworkingV1().Ingresses(i.Namespace)
	if _, err := ingresses.Create(ctx, i, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		current, getErr := ingresses.Get(ctx, i.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("kube: reading ingress %s/%s: %w", i.Namespace, i.Name, getErr)
		}
		current.Spec = i.Spec
		if _, err := ingresses.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("kube: updating ingress %s/%s: %w", i.Namespace, i.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("kube: creating ingress %s/%s: %w", i.Namespace, i.Name, err)
	}
	return nil
}

// updateDeployment writes the composed spec over the live one, keeping the
// resource version the API server needs to detect a concurrent write.
func (c *Client) updateDeployment(ctx context.Context, d *appsv1.Deployment) error {
	api := c.cs.AppsV1().Deployments(d.Namespace)
	current, err := api.Get(ctx, d.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("kube: reading deployment %s/%s: %w", d.Namespace, d.Name, err)
	}
	current.Labels = d.Labels
	current.Spec = d.Spec
	if _, err := api.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("kube: updating deployment %s/%s: %w", d.Namespace, d.Name, err)
	}
	return nil
}

// WorkloadReady reports whether a Deployment's new pods are serving, which is
// the platform's whole definition of an app being up.
//
// Every clause carries weight, because a rolling update keeps the previous pods
// serving while the new ones start. AvailableReplicas on its own is satisfied by
// a pod from the old ReplicaSet, so a rollout that can never succeed, an image
// that does not pull being the plain case, reads as ready within milliseconds
// and a failed deploy is recorded healthy. This is the same set of comparisons
// `kubectl rollout status` makes.
func (c *Client) WorkloadReady(ctx context.Context, namespace, name string) (bool, error) {
	d, err := c.cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("kube: reading deployment %s/%s: %w", namespace, name, err)
	}
	// The controller has not looked at this spec yet, so every count below still
	// describes the previous one.
	if d.Status.ObservedGeneration < d.Generation {
		return false, nil
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	return d.Status.UpdatedReplicas >= want && // every new pod exists
		d.Status.Replicas == d.Status.UpdatedReplicas && // no old pod is left
		d.Status.AvailableReplicas >= d.Status.UpdatedReplicas, nil // and the new ones are available
}

// PodsForApp lists an app's own pods, newest first, with just enough status for
// a log read to decide whether there is anything to fetch.
//
// The namespace and the selector are both derived from the app's slug by the
// caller, so no caller supplied value ever reaches the API as a name, and the
// read cannot leave the app's own namespace (spec 0006, AC-11).
func (c *Client) PodsForApp(ctx context.Context, namespace, slug string) ([]logs.PodStatus, error) {
	sel := labels.SelectorFromSet(deploy.Selector(slug)).String()
	list, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	// An app's namespace and the RoleBinding reaching into it are created at the
	// deploy step, so during the build there is nothing here and Kubernetes says
	// Forbidden rather than empty. Both answers mean the same thing to a log read,
	// and neither is a fault (spec 0006, AC-7).
	if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("kube: listing pods in %s: %w", namespace, logs.ErrNoNamespace)
	}
	if err != nil {
		return nil, fmt.Errorf("kube: listing pods in %s: %w", namespace, err)
	}

	// Newest first, whatever phase each one is in, so a crash looping or pending
	// pod is the one reported rather than an older healthy one (AC-5).
	pods := list.Items
	sort.SliceStable(pods, func(i, j int) bool {
		return pods[j].CreationTimestamp.Before(&pods[i].CreationTimestamp)
	})

	out := make([]logs.PodStatus, 0, len(pods))
	for i := range pods {
		out = append(out, podStatus(&pods[i]))
	}
	return out, nil
}

// podStatus projects the one container an app pod has. An empty container status
// list means no container has started, which is the empty case, never an index
// into position zero (spec 0006, Value sourcing).
func podStatus(pod *corev1.Pod) logs.PodStatus {
	st := logs.PodStatus{Name: pod.Name}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			st.Ready = cond.Status == corev1.ConditionTrue
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != deploy.WorkloadName {
			continue
		}
		st.RestartCount = cs.RestartCount
		st.ContainerStarted = cs.State.Waiting == nil
	}
	return st
}

// PodLog reads one container's tail with the kubelet's own timestamps, either
// the running container or the previous one it replaced after a crash.
//
// The container is the WorkloadName constant rather than anything a caller
// named: an app pod has exactly one container, and which one is read is not a
// decision a caller gets to make (AC-11).
func (c *Client) PodLog(ctx context.Context, namespace, pod string, tailLines int, previous bool) (string, error) {
	tail := int64(tailLines)
	req := c.cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container:  deploy.WorkloadName,
		Timestamps: true,
		TailLines:  &tail,
		Previous:   previous,
	})
	body, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("kube: reading the log of %s/%s: %w", namespace, pod, err)
	}
	defer func() { _ = body.Close() }()

	// Bounded by the tail the caller asked for, but the ceiling here is a second
	// fence: a single record the kubelet never split must not be able to pull the
	// whole response into memory.
	raw, err := io.ReadAll(io.LimitReader(body, logReadCeiling))
	if err != nil {
		return "", fmt.Errorf("kube: reading the log of %s/%s: %w", namespace, pod, err)
	}
	return string(raw), nil
}

// logReadCeiling is the most one log read will pull off the wire, comfortably
// above the block ceilings in internal/logs so bounding stays that package's
// decision rather than an accident of this one.
const logReadCeiling = 8 << 20

// ignoreExists turns an already exists error into success, which is what makes
// every ensure here idempotent.
func ignoreExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
