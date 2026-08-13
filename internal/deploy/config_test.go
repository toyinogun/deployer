package deploy_test

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/deploy"
)

// configInput is the ordinary input with configuration on it.
func configInput(config map[string]string) deploy.Input {
	return deploy.Input{
		AppID:       "app_01",
		Slug:        "hello-a1b2c3",
		Host:        "hello-a1b2c3.deploy.example.org",
		Image:       "registry.example.org/hello@sha256:" + strings.Repeat("a", 64),
		CPU:         "50m",
		Memory:      "64Mi",
		LimitCPU:    "500m",
		LimitMemory: "256Mi",
		Config:      config,
	}
}

func TestSecretHoldsEveryConfiguredValue(t *testing.T) {
	// covers: spec 0010 AC-7, AC-15
	t.Parallel()
	in := configInput(map[string]string{
		"DATABASE_URL": "postgres://localhost/app",
		"EMPTY":        "",
	})
	s := deploy.Secret(in)

	if s.Name != deploy.ConfigSecretName || s.Namespace != deploy.NamespaceName(in.Slug) {
		t.Errorf("secret is %s/%s, want %s in the app's own namespace", s.Namespace, s.Name, deploy.ConfigSecretName)
	}
	if got := string(s.Data["DATABASE_URL"]); got != "postgres://localhost/app" {
		t.Errorf("DATABASE_URL = %q", got)
	}
	// An empty string is a value, not the key being unset, so the key is present
	// and its data is empty (AC-15).
	value, ok := s.Data["EMPTY"]
	if !ok || len(value) != 0 {
		t.Errorf("EMPTY = %q present %v, want an empty value that is still a key", value, ok)
	}
	if len(s.Data) != 2 {
		t.Errorf("secret holds %d keys, want the two configured", len(s.Data))
	}
}

func TestSecretIsComposedEvenWithNoConfiguration(t *testing.T) {
	// covers: spec 0010 AC-7
	t.Parallel()
	// The container references the Secret by name, so an app with nothing
	// configured still needs it to exist or its pod never starts.
	s := deploy.Secret(configInput(nil))
	if s == nil || s.Name != deploy.ConfigSecretName {
		t.Fatalf("an app with no configuration composed %+v", s)
	}
	if len(s.Data) != 0 {
		t.Errorf("secret holds %+v, want nothing", s.Data)
	}
}

func TestSecretNeverCarriesTheReservedKeys(t *testing.T) {
	// covers: spec 0010 AC-5
	t.Parallel()
	// Validation refuses them long before this, so what this holds in place is
	// that the composer adds neither of them itself.
	s := deploy.Secret(configInput(map[string]string{"LOG_LEVEL": "debug"}))
	for _, key := range []string{"PORT", "APP_URL"} {
		if _, found := s.Data[key]; found {
			t.Errorf("the configuration secret carries %s, which the platform composes itself", key)
		}
	}
}

func TestThePodTemplateChecksumFollowsTheConfiguration(t *testing.T) {
	// covers: spec 0010 AC-17
	t.Parallel()
	base := configInput(map[string]string{"A": "1", "B": "2"})
	changed := configInput(map[string]string{"A": "1", "B": "3"})

	first := deploy.Deployment(base).Spec.Template.Annotations[deploy.ConfigChecksumAnnotation]
	same := deploy.Deployment(configInput(map[string]string{"B": "2", "A": "1"})).
		Spec.Template.Annotations[deploy.ConfigChecksumAnnotation]
	other := deploy.Deployment(changed).Spec.Template.Annotations[deploy.ConfigChecksumAnnotation]

	if first == "" {
		t.Fatal("the pod template carries no configuration checksum, so a configuration only deploy would roll nothing")
	}
	// The same configuration in a different map order is the same configuration.
	if first != same {
		t.Errorf("the same configuration hashed to %q and %q", first, same)
	}
	if first == other {
		t.Errorf("changing a value left the checksum at %q, so the pods would not roll", first)
	}
}

func TestTheChecksumDoesNotCarryTheValuesItNames(t *testing.T) {
	// covers: spec 0010 Key invariants
	t.Parallel()
	sum := deploy.ConfigChecksum(map[string]string{"API_KEY": "hunter2-the-real-secret"})
	if strings.Contains(sum, "hunter2") || strings.Contains(sum, "API_KEY") {
		t.Errorf("the checksum carries what it names: %q", sum)
	}
}

func TestTwoDifferentConfigurationsCannotHashTheSame(t *testing.T) {
	// covers: spec 0010 AC-17
	t.Parallel()
	// The obvious ambiguity if the digest were a plain concatenation: the split
	// between a key and its value has to be unambiguous.
	first := deploy.ConfigChecksum(map[string]string{"AB": "C"})
	second := deploy.ConfigChecksum(map[string]string{"A": "BC"})
	if first == second {
		t.Errorf("AB=C and A=BC both hash to %q", first)
	}
}
