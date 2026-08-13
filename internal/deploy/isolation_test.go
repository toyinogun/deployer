package deploy_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/build"
	"github.com/toyinogun/deployer/internal/deploy"
	corev1 "k8s.io/api/core/v1"
)

// Pins the isolation that has been true since spec 0003 and that nothing tested
// as a property. Every field named here is one a privileged workload needs, and
// the composition is where they would appear (spec 0008, AC-17).
func TestComposedPodSpecsCarryNoPrivilegedField(t *testing.T) {
	specs := map[string]corev1.PodSpec{
		"the app deployment": deploy.Deployment(policyInput()).Spec.Template.Spec,
		"the build job":      build.Job(buildInput()).Spec.Template.Spec,
	}
	for name, spec := range specs {
		t.Run(name, func(t *testing.T) {
			if spec.HostNetwork || spec.HostPID || spec.HostIPC {
				t.Errorf("host namespaces are shared: network=%v pid=%v ipc=%v",
					spec.HostNetwork, spec.HostPID, spec.HostIPC)
			}
			for _, v := range spec.Volumes {
				if v.HostPath != nil {
					t.Errorf("volume %q mounts the host path %q", v.Name, v.HostPath.Path)
				}
			}
			if spec.SecurityContext == nil || spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot {
				t.Errorf("the pod does not require a non root user")
			}
			if spec.SecurityContext == nil || spec.SecurityContext.SeccompProfile == nil ||
				spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Errorf("the pod does not carry the runtime default seccomp profile")
			}
			containers := append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...)
			if len(containers) == 0 {
				t.Fatal("no containers at all, so this proves nothing")
			}
			for _, c := range containers {
				assertContainerIsFenced(t, c)
			}
		})
	}
}

func assertContainerIsFenced(t *testing.T, c corev1.Container) {
	t.Helper()
	sc := c.SecurityContext
	if sc == nil {
		t.Fatalf("container %q carries no security context at all", c.Name)
	}
	if sc.Privileged != nil && *sc.Privileged {
		t.Errorf("container %q is privileged", c.Name)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("container %q allows privilege escalation", c.Name)
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("container %q does not require a non root user", c.Name)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("container %q does not carry the runtime default seccomp profile", c.Name)
	}
	// Reported and returned rather than dereferenced: this is the test that
	// exists to catch a container losing its capability rules, so it has to say
	// so plainly instead of panicking on the nil a few lines down.
	if sc.Capabilities == nil {
		t.Errorf("container %q carries no capability rules at all", c.Name)
		return
	}
	if len(sc.Capabilities.Add) != 0 {
		t.Errorf("container %q adds capabilities: %+v", c.Name, sc.Capabilities.Add)
	}
	dropsAll := false
	for _, capability := range sc.Capabilities.Drop {
		if capability == "ALL" {
			dropsAll = true
		}
	}
	if !dropsAll {
		t.Errorf("container %q does not drop ALL capabilities", c.Name)
	}
}

// What makes a privileged request unexpressible rather than merely rejected: an
// Input with nowhere to put one. A map, or a field named for annotations, an
// environment, or an override, is a passthrough however carefully it is filtered
// downstream (spec 0008, AC-18).
func TestDeployInputCarriesNoPassthrough(t *testing.T) {
	forbidden := []string{"annotation", "label", "env", "override", "extra", "spec", "patch", "raw", "custom"}

	typ := reflect.TypeOf(deploy.Input{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		switch field.Type.Kind() {
		case reflect.Map, reflect.Interface, reflect.Pointer, reflect.Struct, reflect.Func:
			// One map is expected. Config holds what a caller set, and it is the
			// exception that proves the rule: it lands in a Secret's data and the
			// pod spec references that Secret by name, so no string a caller sent
			// reaches a pod spec field (spec 0010, Key invariants). Any other map
			// would be a passthrough.
			if field.Type.Kind() != reflect.Map || field.Name != "Config" {
				t.Errorf("field %s is a %s, which can carry a value the platform did not compose",
					field.Name, field.Type.Kind())
			}
		case reflect.Slice:
			// One slice is expected, and it holds configuration the platform
			// parsed at startup, never anything a caller sent.
			if field.Name != "EgressBlockedCIDRs" {
				t.Errorf("field %s is a slice, which is a passthrough unless it is platform derived", field.Name)
			}
		}
		lower := strings.ToLower(field.Name)
		for _, word := range forbidden {
			if strings.Contains(lower, word) {
				t.Errorf("field %s reads like a passthrough (%q)", field.Name, word)
			}
		}
	}
}

// buildInput is one build's composition, in the shape the reconcile loop hands it.
func buildInput() build.Input {
	return build.Input{
		DeploymentID:    "dep_01J000000000000000000000",
		Namespace:       "deployer-builds",
		AppSlug:         "hello-a1b2c3",
		SelfImage:       "ghcr.io/toyinogun/deployer@sha256:" + strings.Repeat("b", 64),
		BuilderImage:    "paketobuildpacks/builder-jammy-base@sha256:" + strings.Repeat("a", 64),
		TargetImage:     "registry.deployer-system:5000/apps/hello-a1b2c3:dep_01J000000000000000000000",
		BuildUID:        1001,
		BuildGID:        1000,
		FetchURL:        "http://deployer.deployer-system.svc/v1/uploads/up_01J000000000000000000000",
		FetchToken:      "token",
		ExpectedSHA:     strings.Repeat("c", 64),
		MaxFiles:        20000,
		MaxExtracted:    512 << 20,
		CredentialRef:   "build-dep-01j000000000000000000000",
		DeadlineSeconds: 480,
	}
}
