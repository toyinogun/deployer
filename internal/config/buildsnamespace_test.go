package config

import (
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The two build namespaces enforce different pod security levels, and which one
// enforces which is load bearing rather than cosmetic (spec 0009, AC-10, AC-20).
//
// deployer-builds at `restricted` is what turns a mis routed BuildKit Job into a
// create failure the platform reports, instead of a build that quietly runs in
// the wrong place. deployer-builds-dockerfile at `privileged` is the only level
// that admits the BuildKit pod at all, and its audit and warn levels stay at
// `baseline` so the four expected deviations are recorded on every build and a
// fifth is visible rather than silent.
//
// ArgoCD applies these labels and no Go code reads them, so relaxing one is a
// change nothing would otherwise notice.
func TestTheBuildNamespacesEnforceTheirOwnLevels(t *testing.T) {
	for _, want := range []struct {
		path, name, enforce, auditAndWarn string
	}{
		{"../../deploy/builds-namespace.yaml", "deployer-builds", "restricted", "restricted"},
		{"../../deploy/builds-dockerfile-namespace.yaml", "deployer-builds-dockerfile", "privileged", "baseline"},
	} {
		t.Run(want.name, func(t *testing.T) {
			ns := namespaceIn(t, want.path)
			if ns.Name != want.name {
				t.Fatalf("%s declares namespace %q, want %s", want.path, ns.Name, want.name)
			}
			for label, value := range map[string]string{
				"pod-security.kubernetes.io/enforce": want.enforce,
				"pod-security.kubernetes.io/audit":   want.auditAndWarn,
				"pod-security.kubernetes.io/warn":    want.auditAndWarn,
			} {
				if got := ns.Labels[label]; got != value {
					t.Errorf("%s = %q, want %q", label, got, value)
				}
			}
		})
	}
}

// namespaceIn reads the one Namespace object out of a file that also holds its
// RoleBinding.
func namespaceIn(t *testing.T, path string) corev1.Namespace {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var found []corev1.Namespace
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var partial struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &partial); err != nil {
			t.Fatalf("parsing a document of %s: %v", path, err)
		}
		if partial.Kind != "Namespace" {
			continue
		}
		var ns corev1.Namespace
		if err := yaml.Unmarshal([]byte(doc), &ns); err != nil {
			t.Fatalf("parsing the namespace in %s: %v", path, err)
		}
		found = append(found, ns)
	}
	if len(found) != 1 {
		t.Fatalf("%s holds %d Namespace objects, want exactly 1", path, len(found))
	}
	return found[0]
}
