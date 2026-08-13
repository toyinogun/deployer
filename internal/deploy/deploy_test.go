package deploy_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/deploy"
	corev1 "k8s.io/api/core/v1"
)

// input is one app's composition, with values distinct enough that a field
// picking up the wrong one is visible.
func input() deploy.Input {
	return deploy.Input{
		AppID:                 "app_01J000000000000000000000",
		Slug:                  "hello-a1b2c3",
		Host:                  "hello-a1b2c3.deploy.example.org",
		Image:                 "registry.deployer-system:5000/apps/hello-a1b2c3@sha256:" + strings.Repeat("a", 64),
		IngressClassName:      "nginx",
		CPU:                   "100m",
		Memory:                "128Mi",
		LimitCPU:              "500m",
		LimitMemory:           "512Mi",
		QuotaCPU:              "1",
		QuotaMemory:           "1Gi",
		QuotaPods:             5,
		ControlPlaneNamespace: "deployer-system",
	}
}

func TestNamespaceCarriesTheFence(t *testing.T) {
	ns := deploy.Namespace(input())

	if ns.Name != "app-hello-a1b2c3" {
		t.Errorf("namespace name = %q, want app-hello-a1b2c3", ns.Name)
	}
	want := map[string]string{
		"app.kubernetes.io/managed-by":               "deployer",
		"deployer.internal/app-id":                   "app_01J000000000000000000000",
		"deployer.internal/app-slug":                 "hello-a1b2c3",
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/enforce-version": "latest",
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/audit":           "restricted",
	}
	for k, v := range want {
		if ns.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, ns.Labels[k], v)
		}
	}
}

func TestRoleBindingPointsAtTheAppRoleOnly(t *testing.T) {
	rb := deploy.RoleBinding(input())

	if rb.Namespace != "app-hello-a1b2c3" {
		t.Errorf("binding namespace = %q, want the app namespace", rb.Namespace)
	}
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != "deployer-app" {
		t.Errorf("roleRef = %s/%s, want ClusterRole/deployer-app", rb.RoleRef.Kind, rb.RoleRef.Name)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Namespace != "deployer-system" {
		t.Errorf("subjects = %+v, want the deployer service account in deployer-system", rb.Subjects)
	}
}

func TestQuotaAndLimitRangeAgree(t *testing.T) {
	in := input()
	quota := deploy.ResourceQuota(in)
	limits := deploy.LimitRange(in)

	if got := quota.Spec.Hard[corev1.ResourcePods]; got.Value() != 5 {
		t.Errorf("pods quota = %s, want 5", got.String())
	}
	if got := quota.Spec.Hard[corev1.ResourceLimitsMemory]; got.String() != "1Gi" {
		t.Errorf("memory limit quota = %s, want 1Gi", got.String())
	}
	// A LimitRange maximum above the quota would admit a pod the quota then
	// refuses, which is the confusing failure this pairing exists to avoid.
	item := limits.Spec.Limits[0]
	if got := item.Max[corev1.ResourceMemory]; got.String() != "1Gi" {
		t.Errorf("limit range max memory = %s, want the quota's 1Gi", got.String())
	}
	if got := item.Default[corev1.ResourceCPU]; got.String() != "500m" {
		t.Errorf("default cpu = %s, want the configured limit 500m", got.String())
	}
	if got := item.DefaultRequest[corev1.ResourceCPU]; got.String() != "100m" {
		t.Errorf("default cpu request = %s, want the configured request 100m", got.String())
	}
}

func TestDeploymentIsRestrictedAndRunsTheDigest(t *testing.T) {
	in := input()
	d := deploy.Deployment(in)

	if *d.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", *d.Spec.Replicas)
	}
	if len(d.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(d.Spec.Template.Spec.Containers))
	}
	c := d.Spec.Template.Spec.Containers[0]

	if c.Image != in.Image || !strings.Contains(c.Image, "@sha256:") {
		t.Errorf("image = %q, want the digest reference", c.Image)
	}
	// The two variables the platform owns, listed after envFrom so they win over
	// anything the app's own configuration holds (spec 0010, AC-5, AC-7).
	if len(c.Env) != 2 || c.Env[0].Name != "PORT" || c.Env[0].Value != "8080" {
		t.Errorf("env = %+v, want PORT=8080 first", c.Env)
	}
	if c.Env[1].Name != "APP_URL" || c.Env[1].Value != "https://"+in.Host {
		t.Errorf("env = %+v, want APP_URL built from the ingress host", c.Env)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil ||
		c.EnvFrom[0].SecretRef.Name != deploy.ConfigSecretName {
		t.Errorf("envFrom = %+v, want the app's configuration Secret", c.EnvFrom)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("ports = %+v, want just 8080", c.Ports)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil {
		t.Fatal("readiness must be a TCP socket probe")
	}
	if got := c.ReadinessProbe.TCPSocket.Port.IntValue(); got != 8080 {
		t.Errorf("readiness port = %d, want 8080", got)
	}
	if c.ReadinessProbe.HTTPGet != nil {
		t.Error("readiness must not assume an HTTP path the platform knows nothing about")
	}

	// Every field `restricted` pod security refuses a pod for omitting.
	pod := d.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("pod must declare runAsNonRoot")
	}
	if pod.SecurityContext.SeccompProfile == nil ||
		pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("pod must declare the RuntimeDefault seccomp profile")
	}
	if c.SecurityContext == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("container must refuse privilege escalation")
	}
	if len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities drop = %v, want [ALL]", c.SecurityContext.Capabilities.Drop)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("an app must hold no Kubernetes credential")
	}

	// The pull secret is referenced and nothing more: not a mount, not a
	// projection, not an environment variable (AC-12).
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != deploy.PullSecretName {
		t.Errorf("imagePullSecrets = %+v, want just %s", pod.ImagePullSecrets, deploy.PullSecretName)
	}
	if len(pod.Volumes) != 0 || len(c.VolumeMounts) != 0 {
		t.Error("an app pod mounts nothing, so no secret can reach its filesystem")
	}
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			t.Errorf("env %s reads from a cluster object, which no app may do here", e.Name)
		}
	}
}

func TestServiceAndIngressMatchTheContract(t *testing.T) {
	in := input()
	svc := deploy.Service(in)
	ing := deploy.Ingress(in)

	if svc.Name != "app" {
		t.Errorf("service name = %q, want app", svc.Name)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 {
		t.Errorf("service ports = %+v, want 80", svc.Spec.Ports)
	}
	if got := svc.Spec.Ports[0].TargetPort.String(); got != "http" {
		t.Errorf("service target = %q, want the container's http port", got)
	}

	if len(ing.Spec.TLS) != 0 {
		t.Error("an app Ingress carries no tls block, so no app namespace holds a key")
	}
	if *ing.Spec.IngressClassName != "nginx" {
		t.Errorf("ingress class = %q, want nginx", *ing.Spec.IngressClassName)
	}
	if ing.Spec.Rules[0].Host != in.Host {
		t.Errorf("host = %q, want %q", ing.Spec.Rules[0].Host, in.Host)
	}
	backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if backend.Name != "app" || backend.Port.Number != 80 {
		t.Errorf("backend = %s:%d, want app:80", backend.Name, backend.Port.Number)
	}
}

// The invariant the whole package exists for: the only caller derived strings in
// a composed pod spec are the slug the platform derived and the digest it
// computed. A name an agent chose must appear nowhere.
func TestNoCallerSuppliedValueReachesThePodSpec(t *testing.T) {
	in := input()
	encoded, err := json.Marshal(deploy.Deployment(in).Spec.Template.Spec)
	if err != nil {
		t.Fatalf("encoding the pod spec: %v", err)
	}
	spec := string(encoded)

	for _, forbidden := range []string{
		"My Cool App",                      // the name an agent sends
		"registry-user", "registry-secret", // registry credentials
		in.AppID, // the row id belongs on the namespace, not in the pod
	} {
		if strings.Contains(spec, forbidden) {
			t.Errorf("pod spec contains %q", forbidden)
		}
	}
	if !strings.Contains(spec, in.Slug) || !strings.Contains(spec, "@sha256:") {
		t.Error("pod spec should carry the slug and the digest, which are the two values that belong")
	}
}
