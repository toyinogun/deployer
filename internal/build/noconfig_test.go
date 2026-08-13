package build_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/build"
	corev1 "k8s.io/api/core/v1"
)

// A build receives no configuration at all, on either path. This is a deliberate
// absence rather than an oversight: a value that reaches a build layer is baked
// into a published image, and an app's configuration belongs to the running app
// (spec 0010, AC-14).
//
// The absence needs something holding it in place, because adding a build
// argument later is a one line change nothing else would notice.
func TestNoBuildInputCanCarryConfiguration(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(build.Input{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		if kind := field.Type.Kind(); kind == reflect.Map || kind == reflect.Slice {
			t.Errorf("field %s is a %s, which could carry configuration into a build", field.Name, kind)
		}
		lower := strings.ToLower(field.Name)
		for _, word := range []string{"config", "env", "buildarg"} {
			// CredentialRef is the per Job registry login: the platform's own, and
			// named for what it is.
			if strings.Contains(lower, word) && field.Name != "CredentialRef" {
				t.Errorf("field %s reads like a way to pass configuration to a build (%q)", field.Name, word)
			}
		}
	}
}

func TestNeitherBuildPathGivesAContainerAnAppsConfiguration(t *testing.T) {
	// covers: spec 0010 AC-14
	t.Parallel()
	for name, in := range map[string]build.Input{"buildpacks": input(), "dockerfile": dockerfileInput()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pod := build.Job(in).Spec.Template.Spec
			for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
				// Nothing is read wholesale, so no Secret can become a build's
				// environment by being referenced rather than named.
				if len(c.EnvFrom) != 0 {
					t.Errorf("container %s reads %+v wholesale into its environment", c.Name, c.EnvFrom)
				}
				for _, e := range c.Env {
					if !platformEnv(e.Name) {
						t.Errorf("container %s carries %s, which is not one of the platform's own variables", c.Name, e.Name)
					}
				}
				for _, arg := range append(append([]string{}, c.Command...), c.Args...) {
					for _, flag := range []string{"--build-arg", "--secret"} {
						if strings.Contains(arg, flag) {
							t.Errorf("container %s passes %q, so a value could reach a build layer", c.Name, arg)
						}
					}
				}
			}
		})
	}
}

// platformEnv is the closed set of variables a build container may hold. Every
// one of them is the platform's own; none comes from an app's configuration, and
// a new one has to be added here deliberately rather than slipping in.
func platformEnv(name string) bool {
	switch name {
	case "DEPLOYER_FETCH_URL", "DEPLOYER_FETCH_TOKEN", "DEPLOYER_EXPECTED_SHA256",
		"DEPLOYER_SOURCE_DIR", "DEPLOYER_MAX_UPLOAD_FILES", "DEPLOYER_MAX_EXTRACTED_BYTES",
		"DOCKER_CONFIG", "CNB_PLATFORM_API", "CNB_INSECURE_REGISTRIES",
		"BUILDKITD_FLAGS", "HOME", "XDG_RUNTIME_DIR", "TMPDIR":
		return true
	}
	return false
}
