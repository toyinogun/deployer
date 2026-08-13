// Package deploy composes everything an app needs to run: its namespace and the
// fence around it, the RoleBinding that gives the control plane reach into it,
// and the Deployment, Service, and Ingress that serve it.
//
// Field by field construction over the Kubernetes API types, the same rule the
// build Job follows: no templating, no YAML strings, and no value a caller
// supplied reaching a pod spec. The only caller derived values anywhere here are
// the slug, which the platform derived itself, and the image digest, which the
// platform computed from a build it ran (spec 0004, Key invariants).
//
// It imports the API types but never a client, so composing an app's objects is
// testable without a cluster.
package deploy

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// The platform constants every app runs under. None of them is configurable and
// none of them comes from a caller: an app fits the platform, not the reverse.
const (
	// ContainerPort is the one port an app may listen on, handed to it as PORT.
	ContainerPort = int32(8080)

	// WorkloadName names the Deployment, the Service, and the Ingress. Each lives
	// alone in its app's namespace, so there is nothing to disambiguate.
	WorkloadName = "app"

	// ServicePort is where the Service listens, in front of ContainerPort.
	ServicePort = int32(80)

	// PullSecretName is the registry credential the kubelet pulls with. Refreshed
	// on every deploy, never mounted into the app container (AC-12).
	PullSecretName = "registry"

	// RoleBindingName is the control plane's reach into one app namespace. It
	// binds ClusterRole/deployer-app, which is bound nowhere cluster wide, so the
	// platform's rights are exactly the set of app namespaces that exist.
	RoleBindingName = "deployer"

	// AppRole is the ClusterRole the binding points at (deploy/rbac.yaml).
	AppRole = "deployer-app"

	quotaName      = "app-quota"
	limitRangeName = "app-defaults"

	// Quota headroom the platform sets rather than configures. An app gets one
	// Service and a handful of Secrets; more than that is a different feature.
	quotaServices = "3"
	quotaSecrets  = "10"
)

// managedBy marks everything the platform owns, so a human reading the cluster
// can tell what was composed from what was applied.
const managedBy = "app.kubernetes.io/managed-by"

// Input is everything an app's objects are composed from. Every field is
// platform derived or configuration; none of it is a string a caller sent.
type Input struct {
	AppID string // the apps row id, recorded as a namespace label
	Slug  string // the platform derived slug, which names the namespace and the host
	Host  string // slug plus DEPLOYER_APP_DOMAIN

	// Image is the digest reference the container runs, repo@sha256:..., never
	// the tag the build pushed under.
	Image string

	IngressClassName string

	CPU         string // DEPLOYER_APP_DEFAULT_CPU
	Memory      string // DEPLOYER_APP_DEFAULT_MEMORY
	LimitCPU    string // DEPLOYER_APP_LIMIT_CPU
	LimitMemory string // DEPLOYER_APP_LIMIT_MEMORY

	QuotaCPU    string // DEPLOYER_APP_QUOTA_CPU
	QuotaMemory string // DEPLOYER_APP_QUOTA_MEMORY
	QuotaPods   int    // DEPLOYER_APP_QUOTA_PODS

	// ControlPlaneNamespace is where the deployer ServiceAccount lives, which is
	// the subject of the RoleBinding this creates.
	ControlPlaneNamespace string

	// EgressBlockedCIDRs is DEPLOYER_APP_EGRESS_BLOCKED_CIDRS, already parsed at
	// startup. It becomes the `except` list of the egress allow rule and is the
	// whole of an app's isolation from other apps and from the cluster.
	EgressBlockedCIDRs []string
}

// NamespaceName is the namespace one app lives in.
func NamespaceName(slug string) string { return "app-" + slug }

// Namespace composes an app's namespace from spec 0003's template: ownership
// labels and the restricted pod security labels the API server enforces.
//
// Created when it does not exist and left untouched when it does. The platform
// never edits an existing app namespace's labels, quota, or limits, because
// those are the fence and a fence the platform can move is not a fence.
func Namespace(in Input) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: NamespaceName(in.Slug),
			Labels: map[string]string{
				managedBy:                    "deployer",
				"deployer.internal/app-id":   in.AppID,
				"deployer.internal/app-slug": in.Slug,
				// Enforced by the API server, not by platform code, so it holds
				// even if the composition above it has a bug.
				"pod-security.kubernetes.io/enforce":         "restricted",
				"pod-security.kubernetes.io/enforce-version": "latest",
				"pod-security.kubernetes.io/warn":            "restricted",
				"pod-security.kubernetes.io/audit":           "restricted",
			},
		},
	}
}

// RoleBinding gives the control plane its rights inside one app namespace, and
// nowhere else.
func RoleBinding(in Input) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: objectMeta(RoleBindingName, in.Slug),
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     AppRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      "deployer",
			Namespace: in.ControlPlaneNamespace,
		}},
	}
}

// ResourceQuota bounds what one app may take from the cluster.
func ResourceQuota(in Input) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: objectMeta(quotaName, in.Slug),
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU:    resource.MustParse(in.QuotaCPU),
				corev1.ResourceLimitsCPU:      resource.MustParse(in.QuotaCPU),
				corev1.ResourceRequestsMemory: resource.MustParse(in.QuotaMemory),
				corev1.ResourceLimitsMemory:   resource.MustParse(in.QuotaMemory),
				corev1.ResourcePods:           *resource.NewQuantity(int64(in.QuotaPods), resource.DecimalSI),
				corev1.ResourceServices:       resource.MustParse(quotaServices),
				corev1.ResourceSecrets:        resource.MustParse(quotaSecrets),
			},
		},
	}
}

// LimitRange is required rather than optional. A ResourceQuota that constrains
// limits makes the API server reject any pod declaring neither requests nor
// limits, which is most images, so these defaults are what keep an ordinary pod
// admissible (spec 0003, AC-7).
func LimitRange(in Input) *corev1.LimitRange {
	return &corev1.LimitRange{
		ObjectMeta: objectMeta(limitRangeName, in.Slug),
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{{
				Type: corev1.LimitTypeContainer,
				Default: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(in.LimitCPU),
					corev1.ResourceMemory: resource.MustParse(in.LimitMemory),
				},
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(in.CPU),
					corev1.ResourceMemory: resource.MustParse(in.Memory),
				},
				Max: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(in.QuotaCPU),
					corev1.ResourceMemory: resource.MustParse(in.QuotaMemory),
				},
			}},
		},
	}
}

// Deployment composes the workload itself.
//
// The container references the image by digest, carries spec 0003's required
// security context, and holds exactly one environment variable. Readiness is a
// TCP probe rather than an HTTP one on purpose: the platform knows the port an
// app was told to listen on and nothing about what it serves there (AC-14).
func Deployment(in Input) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: objectMeta(WorkloadName, in.Slug),
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: Selector(in.Slug)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels(in.Slug)},
				Spec: corev1.PodSpec{
					// The app holds no Kubernetes rights at all. It is a web
					// server the platform happens to have built.
					AutomountServiceAccountToken: ptr(false),
					ImagePullSecrets:             []corev1.LocalObjectReference{{Name: PullSecretName}},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{container(in)},
				},
			},
		},
	}
}

// container is the one container an app runs.
func container(in Input) corev1.Container {
	return corev1.Container{
		Name:  WorkloadName,
		Image: in.Image,
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: ContainerPort}},
		// The whole environment. Application configuration is slice 7; until
		// then an app is told its port and nothing else.
		Env: []corev1.EnvVar{{Name: "PORT", Value: "8080"}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			RunAsNonRoot:             ptr(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(ContainerPort)},
			},
			InitialDelaySeconds: 2,
			PeriodSeconds:       3,
			FailureThreshold:    20,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(in.CPU),
				corev1.ResourceMemory: resource.MustParse(in.Memory),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(in.LimitCPU),
				corev1.ResourceMemory: resource.MustParse(in.LimitMemory),
			},
		},
	}
}

// Service fronts the pod on the port the Ingress routes to.
func Service(in Input) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: objectMeta(WorkloadName, in.Slug),
		Spec: corev1.ServiceSpec{
			Selector: Selector(in.Slug),
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       ServicePort,
				TargetPort: intstr.FromString("http"),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// Ingress publishes the app on its hostname.
//
// No tls block, on purpose: the controller's default wildcard certificate covers
// every app host, so no app namespace ever holds a private key (spec 0003, AC-9).
func Ingress(in Input) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: objectMeta(WorkloadName, in.Slug),
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr(in.IngressClassName),
			Rules: []networkingv1.IngressRule{{
				Host: in.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: WorkloadName,
									Port: networkingv1.ServiceBackendPort{Number: ServicePort},
								},
							},
						}},
					},
				},
			}},
		},
	}
}

// Selector is what the Deployment matches and the Service routes to. It is
// immutable on a Deployment, so it holds only the slug, which never changes for
// the life of an app. Exported because reading an app's own pods happens in
// internal/kube, which composes nothing of its own (spec 0006, AC-11).
func Selector(slug string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": slug}
}

// podLabels are the selector plus ownership, which the selector may not carry
// because a selector cannot change and ownership labels might.
func podLabels(slug string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name": slug,
		managedBy:                "deployer",
	}
}

// objectMeta is the name, namespace, and ownership every composed object shares.
func objectMeta(name, slug string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: NamespaceName(slug),
		Labels: map[string]string{
			managedBy:                "deployer",
			"app.kubernetes.io/name": slug,
		},
	}
}

// ptr returns a pointer to v, for the many optional fields in a spec.
func ptr[T any](v T) *T { return &v }
