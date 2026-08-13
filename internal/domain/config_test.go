package domain_test

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
)

// set is the shorthand every case here builds its call from: a key, a value, and
// a secret flag that is present.
func set(key, value string, secret bool) domain.ConfigWrite {
	return domain.ConfigWrite{Key: key, Value: value, Secret: &secret}
}

func TestValidateConfigAcceptsAnOrdinaryCall(t *testing.T) {
	// covers: AC-1, AC-15
	t.Parallel()
	writes := []domain.ConfigWrite{
		set("DATABASE_URL", "postgres://localhost/app", true),
		set("LOG_LEVEL", "debug", false),
		set("FEATURE_X", "", false),
	}
	if r := domain.ValidateConfig(writes, nil); r != "" {
		t.Fatalf("refused an ordinary call with %q", r)
	}
}

func TestValidateConfigRefusesAnEmptyCall(t *testing.T) {
	t.Parallel()
	if r := domain.ValidateConfig(nil, nil); r != domain.ReasonConfigKeyInvalid {
		t.Fatalf("an empty call gave %q, want %q", r, domain.ReasonConfigKeyInvalid)
	}
}

func TestValidateConfigRefusesAKeyThatIsNotAnEnvironmentVariableName(t *testing.T) {
	// covers: AC-4
	t.Parallel()
	for _, key := range []string{"", "lower", "1LEADING", "HAS-DASH", "HAS SPACE", "HAS.DOT", "café"} {
		writes := []domain.ConfigWrite{set(key, "v", false)}
		if r := domain.ValidateConfig(writes, nil); r != domain.ReasonConfigKeyInvalid {
			t.Errorf("key %q gave %q, want %q", key, r, domain.ReasonConfigKeyInvalid)
		}
	}
}

func TestValidateConfigRefusesTheReservedKeys(t *testing.T) {
	// covers: AC-5
	t.Parallel()
	for _, key := range []string{"PORT", "APP_URL"} {
		writes := []domain.ConfigWrite{set(key, "v", false)}
		if r := domain.ValidateConfig(writes, nil); r != domain.ReasonConfigKeyReserved {
			t.Errorf("reserved key %q gave %q, want %q", key, r, domain.ReasonConfigKeyReserved)
		}
	}
}

func TestValidateConfigRefusesAKeySentWithoutItsSecretFlag(t *testing.T) {
	// covers: AC-16
	t.Parallel()
	writes := []domain.ConfigWrite{{Key: "TOKEN", Value: "abc"}}
	if r := domain.ValidateConfig(writes, nil); r != domain.ReasonConfigFlagMissing {
		t.Fatalf("a missing flag gave %q, want %q", r, domain.ReasonConfigFlagMissing)
	}
}

func TestValidateConfigRefusesTooManyKeys(t *testing.T) {
	// covers: AC-6
	t.Parallel()
	existing := map[string]string{}
	for i := range domain.MaxConfigKeys {
		existing[keyN(i)] = "v"
	}
	writes := []domain.ConfigWrite{set("ONE_MORE", "v", false)}
	if r := domain.ValidateConfig(writes, existing); r != domain.ReasonConfigTooManyKeys {
		t.Fatalf("the sixty fifth key gave %q, want %q", r, domain.ReasonConfigTooManyKeys)
	}
	// Replacing a key the app already has is not a new key.
	replace := []domain.ConfigWrite{set(keyN(0), "other", false)}
	if r := domain.ValidateConfig(replace, existing); r != "" {
		t.Fatalf("replacing an existing key at the bound gave %q, want no refusal", r)
	}
}

func TestValidateConfigRefusesAValueOverTheSingleValueBound(t *testing.T) {
	// covers: AC-6
	t.Parallel()
	writes := []domain.ConfigWrite{set("BIG", strings.Repeat("x", domain.MaxConfigValueBytes+1), false)}
	if r := domain.ValidateConfig(writes, nil); r != domain.ReasonConfigTooLarge {
		t.Fatalf("an oversized value gave %q, want %q", r, domain.ReasonConfigTooLarge)
	}
}

func TestValidateConfigRefusesATotalOverTheWholeConfigBound(t *testing.T) {
	// covers: AC-6
	t.Parallel()
	existing := map[string]string{}
	for i := range 8 {
		existing[keyN(i)] = strings.Repeat("x", domain.MaxConfigValueBytes)
	}
	writes := []domain.ConfigWrite{set("LAST", strings.Repeat("x", domain.MaxConfigValueBytes), false)}
	if r := domain.ValidateConfig(writes, existing); r != domain.ReasonConfigTooLarge {
		t.Fatalf("an oversized total gave %q, want %q", r, domain.ReasonConfigTooLarge)
	}
}

func TestValidateConfigRefusesTheWholeCallOnOneBadKey(t *testing.T) {
	// covers: AC-4, AC-6
	t.Parallel()
	writes := []domain.ConfigWrite{
		set("GOOD_ONE", "v", false),
		set("GOOD_TWO", "v", false),
		set("bad", "v", false),
	}
	if r := domain.ValidateConfig(writes, nil); r != domain.ReasonConfigKeyInvalid {
		t.Fatalf("one bad key among three gave %q, want %q", r, domain.ReasonConfigKeyInvalid)
	}
}

func TestValidateConfigRefusesTheSameKeyTwiceInOneCall(t *testing.T) {
	t.Parallel()
	writes := []domain.ConfigWrite{set("DUPE", "a", false), set("DUPE", "b", true)}
	if r := domain.ValidateConfig(writes, nil); r != domain.ReasonConfigKeyInvalid {
		t.Fatalf("a duplicated key gave %q, want %q", r, domain.ReasonConfigKeyInvalid)
	}
}

func TestValidateUnsetChecksTheKeyShapeToo(t *testing.T) {
	// covers: AC-3, AC-4
	t.Parallel()
	if r := domain.ValidateUnset([]string{"GOOD", "bad"}); r != domain.ReasonConfigKeyInvalid {
		t.Fatalf("a bad key gave %q, want %q", r, domain.ReasonConfigKeyInvalid)
	}
	if r := domain.ValidateUnset(nil); r != domain.ReasonConfigKeyInvalid {
		t.Fatalf("an empty call gave %q, want %q", r, domain.ReasonConfigKeyInvalid)
	}
	if r := domain.ValidateUnset([]string{"GOOD", "ALSO_GOOD"}); r != "" {
		t.Fatalf("an ordinary unset gave %q, want no refusal", r)
	}
}

// keyN is a distinct valid key per index, for the bound cases.
func keyN(i int) string {
	return "K" + strings.Repeat("A", i/26) + string(rune('A'+i%26))
}
