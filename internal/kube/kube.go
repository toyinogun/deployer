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

	"github.com/toyinogun/deployer/internal/build"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// ApplySecret creates or updates one secret. The pull secret is refreshed on
// every deploy through this, so a rotated registry credential reaches an app
// namespace without anything being deleted.
func (c *Client) ApplySecret(ctx context.Context, s *corev1.Secret) error {
	api := c.cs.CoreV1().Secrets(s.Namespace)
	_, err := api.Create(ctx, s, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
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

	services := c.cs.CoreV1().Services(s.Namespace)
	if _, err := services.Create(ctx, s, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		// A Service holds a cluster IP the API server assigned, so the existing
		// spec is edited rather than replaced wholesale.
		current, getErr := services.Get(ctx, s.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("kube: reading service %s/%s: %w", s.Namespace, s.Name, getErr)
		}
		current.Spec.Selector = s.Spec.Selector
		current.Spec.Ports = s.Spec.Ports
		if _, err := services.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("kube: updating service %s/%s: %w", s.Namespace, s.Name, err)
		}
	} else if err != nil {
		return fmt.Errorf("kube: creating service %s/%s: %w", s.Namespace, s.Name, err)
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

// WorkloadReady reports whether a Deployment has an updated replica that is
// available, which is the platform's whole definition of an app being up.
func (c *Client) WorkloadReady(ctx context.Context, namespace, name string) (bool, error) {
	d, err := c.cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("kube: reading deployment %s/%s: %w", namespace, name, err)
	}
	return d.Status.UpdatedReplicas > 0 && d.Status.AvailableReplicas > 0 &&
		d.Status.ObservedGeneration >= d.Generation, nil
}

// ignoreExists turns an already exists error into success, which is what makes
// every ensure here idempotent.
func ignoreExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
