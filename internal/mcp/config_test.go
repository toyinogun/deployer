package mcp

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
)

// configServer is a tool surface over one app the account owns.
func configServer() (*Server, *silentAuditor, auth.Account) {
	account := auth.Account{ID: "acc_1", Name: "bootstrap"}
	apps := &stubApps{existing: map[string]App{"hello": {ID: "app_1", Slug: "hello-a1b2c3", Name: "hello"}}}
	s, auditor := server(apps, &stubDeployments{}, liveUpload(account.ID))
	return s, auditor, account
}

// entry is a caller's config map entry with its flag present.
func entry(value string, secret bool) configValue {
	return configValue{Value: value, Secret: &secret}
}

// valueOf reads one key out of a response.
func valueOf(out configOutput, key string) (configEntryOut, bool) {
	for _, e := range out.Config {
		if e.Key == key {
			return e, true
		}
	}
	return configEntryOut{}, false
}

func TestSetConfigWritesTheKeysAndReturnsTheResult(t *testing.T) {
	// covers: AC-1, AC-8
	s, _, account := configServer()

	_, out, err := s.setConfig(t.Context(), account, setConfigInput{
		Name: "hello",
		Config: map[string]configValue{
			"LOG_LEVEL":    entry("debug", false),
			"DATABASE_URL": entry("postgres://db/app", true),
		},
	})
	if err != nil {
		t.Fatalf("set_config: %v", err)
	}
	if len(out.Config) != 2 {
		t.Fatalf("the response lists %+v, want both keys", out.Config)
	}
	// The change starts nothing, and the response is the only place a caller
	// finds that out (AC-8).
	if !out.AppliesOnNextDeploy || out.Note == "" {
		t.Errorf("the response does not say the change waits for the next deploy: %+v", out)
	}

	// A second call merges rather than replaces.
	_, out, err = s.setConfig(t.Context(), account, setConfigInput{
		Name:   "hello",
		Config: map[string]configValue{"EXTRA": entry("1", false)},
	})
	if err != nil {
		t.Fatalf("the second set_config: %v", err)
	}
	if len(out.Config) != 3 {
		t.Errorf("after merging the app holds %+v, want all three keys", out.Config)
	}
}

func TestASecretValueNeverComesBackFromAnyTool(t *testing.T) {
	// covers: AC-2
	s, _, account := configServer()
	ctx := t.Context()

	_, setOut, err := s.setConfig(ctx, account, setConfigInput{
		Name:   "hello",
		Config: map[string]configValue{"API_KEY": entry("hunter2", true), "PLAIN": entry("visible", false)},
	})
	if err != nil {
		t.Fatalf("set_config: %v", err)
	}
	_, getOut, err := s.getConfig(ctx, account, getConfigInput{Name: "hello"})
	if err != nil {
		t.Fatalf("get_config: %v", err)
	}

	// Including the response to the call that just set it.
	for name, out := range map[string]configOutput{"set_config": setOut, "get_config": getOut} {
		secret, ok := valueOf(out, "API_KEY")
		if !ok {
			t.Fatalf("%s does not list API_KEY at all", name)
		}
		if !secret.Secret || secret.Value != nil {
			t.Errorf("%s returned the secret as %+v, want the flag and a null value", name, secret)
		}
		plain, _ := valueOf(out, "PLAIN")
		if plain.Value == nil || *plain.Value != "visible" {
			t.Errorf("%s withheld a value that is not secret: %+v", name, plain)
		}
	}
}

func TestAnEmptyValueIsAValueAndNotAnUnsetKey(t *testing.T) {
	// covers: AC-15
	s, _, account := configServer()
	_, out, err := s.setConfig(t.Context(), account, setConfigInput{
		Name:   "hello",
		Config: map[string]configValue{"FEATURE": entry("", false)},
	})
	if err != nil {
		t.Fatalf("set_config: %v", err)
	}
	got, ok := valueOf(out, "FEATURE")
	if !ok || got.Value == nil || *got.Value != "" {
		t.Errorf("the empty value came back as %+v present %v, want a present empty value", got, ok)
	}
}

func TestARefusedSetConfigWritesNothing(t *testing.T) {
	// covers: AC-4, AC-5, AC-6, AC-16
	cases := map[string]struct {
		config map[string]configValue
		reason domain.Reason
	}{
		"a key that is not an environment variable name": {
			config: map[string]configValue{"GOOD": entry("1", false), "bad key": entry("2", false)},
			reason: domain.ReasonConfigKeyInvalid,
		},
		"the reserved PORT": {
			config: map[string]configValue{"PORT": entry("9000", false)},
			reason: domain.ReasonConfigKeyReserved,
		},
		"the reserved APP_URL": {
			config: map[string]configValue{"APP_URL": entry("https://elsewhere", false)},
			reason: domain.ReasonConfigKeyReserved,
		},
		"a key with no secret flag": {
			config: map[string]configValue{"TOKEN": {Value: "abc"}},
			reason: domain.ReasonConfigFlagMissing,
		},
		"a value past the size bound": {
			config: map[string]configValue{
				"OK":  entry("1", false),
				"BIG": entry(strings.Repeat("x", domain.MaxConfigValueBytes+1), false),
			},
			reason: domain.ReasonConfigTooLarge,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, account := configServer()
			ctx := t.Context()
			if _, _, err := s.setConfig(ctx, account, setConfigInput{
				Name:   "hello",
				Config: map[string]configValue{"BEFORE": entry("kept", false)},
			}); err != nil {
				t.Fatalf("seeding the configuration: %v", err)
			}

			_, _, err := s.setConfig(ctx, account, setConfigInput{Name: "hello", Config: tc.config})
			if err == nil || !strings.HasPrefix(err.Error(), string(tc.reason)) {
				t.Fatalf("the call answered %v, want %s", err, tc.reason)
			}
			// Nothing from the refused call landed, and what was there survived.
			_, out, err := s.getConfig(ctx, account, getConfigInput{Name: "hello"})
			if err != nil {
				t.Fatalf("get_config: %v", err)
			}
			if len(out.Config) != 1 {
				t.Errorf("the refused call left %+v behind", out.Config)
			}
		})
	}
}

func TestReSettingAnExistingKeyWithoutTheFlagLeavesItSecret(t *testing.T) {
	// covers: AC-16
	s, _, account := configServer()
	ctx := t.Context()
	if _, _, err := s.setConfig(ctx, account, setConfigInput{
		Name:   "hello",
		Config: map[string]configValue{"API_KEY": entry("hunter2", true)},
	}); err != nil {
		t.Fatalf("setting the secret: %v", err)
	}

	_, _, err := s.setConfig(ctx, account, setConfigInput{
		Name:   "hello",
		Config: map[string]configValue{"API_KEY": {Value: "hunter3"}},
	})
	if err == nil || !strings.HasPrefix(err.Error(), string(domain.ReasonConfigFlagMissing)) {
		t.Fatalf("re setting without the flag answered %v, want %s", err, domain.ReasonConfigFlagMissing)
	}
	_, out, err := s.getConfig(ctx, account, getConfigInput{Name: "hello"})
	if err != nil {
		t.Fatalf("get_config: %v", err)
	}
	got, _ := valueOf(out, "API_KEY")
	if !got.Secret || got.Value != nil {
		t.Errorf("the key is now %+v, want it still secret with no value", got)
	}
}

func TestUnsetConfigRemovesEveryKeyOrNoneOfThem(t *testing.T) {
	// covers: AC-3
	s, _, account := configServer()
	ctx := t.Context()
	if _, _, err := s.setConfig(ctx, account, setConfigInput{
		Name:   "hello",
		Config: map[string]configValue{"ONE": entry("1", false), "TWO": entry("2", false)},
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	_, _, err := s.unsetConfig(ctx, account, unsetConfigInput{Name: "hello", Keys: []string{"ONE", "NEVER_SET"}})
	if err == nil || !strings.HasPrefix(err.Error(), string(domain.ReasonConfigKeyUnknown)) {
		t.Fatalf("unsetting an unset key answered %v, want %s", err, domain.ReasonConfigKeyUnknown)
	}
	_, out, err := s.getConfig(ctx, account, getConfigInput{Name: "hello"})
	if err != nil {
		t.Fatalf("get_config: %v", err)
	}
	if len(out.Config) != 2 {
		t.Fatalf("the refused unset left %+v, want both keys", out.Config)
	}

	_, out, err = s.unsetConfig(ctx, account, unsetConfigInput{Name: "hello", Keys: []string{"ONE", "TWO"}})
	if err != nil {
		t.Fatalf("unset_config: %v", err)
	}
	if len(out.Config) != 0 {
		t.Errorf("after unsetting both the app still holds %+v", out.Config)
	}
	if !out.AppliesOnNextDeploy {
		t.Error("unset_config does not say the change waits for the next deploy")
	}
}

func TestEveryConfigToolRefusesAnAppTheCallerDoesNotOwn(t *testing.T) {
	// covers: AC-13
	s, auditor, _ := configServer()
	ctx := t.Context()
	// The app exists; this account is simply not the one that owns it, which is
	// the same answer as it not existing at all.
	stranger := auth.Account{ID: "acc_2"}
	strangerApps := &stubApps{}
	s.apps = strangerApps

	_, _, setErr := s.setConfig(ctx, stranger, setConfigInput{
		Name: "hello", Config: map[string]configValue{"A": entry("1", false)},
	})
	_, _, unsetErr := s.unsetConfig(ctx, stranger, unsetConfigInput{Name: "hello", Keys: []string{"A"}})
	_, _, getErr := s.getConfig(ctx, stranger, getConfigInput{Name: "hello"})

	for name, err := range map[string]error{"set": setErr, "unset": unsetErr, "get": getErr} {
		if err == nil || !strings.HasPrefix(err.Error(), string(domain.ReasonAppUnknown)) {
			t.Errorf("%s_config answered %v, want %s", name, err, domain.ReasonAppUnknown)
		}
	}
	if len(strangerApps.config) != 0 {
		t.Errorf("a refused call wrote %+v", strangerApps.config)
	}
	// Every refusal is an access decision, so every one of them is recorded.
	refusals := 0
	for _, row := range auditor.rows {
		if row.Reason == string(domain.ReasonAppUnknown) {
			refusals++
		}
	}
	if refusals != 3 {
		t.Errorf("recorded %d refusals, want one per tool", refusals)
	}
}

func TestAConfigurationChangeIsAuditedPerKeyAndNeverCarriesAValue(t *testing.T) {
	// covers: AC-12
	s, auditor, account := configServer()
	ctx := t.Context()

	if _, _, err := s.setConfig(ctx, account, setConfigInput{
		Name:   "hello",
		Config: map[string]configValue{"ONE": entry("1", false), "API_KEY": entry("hunter2", true)},
	}); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	if _, _, err := s.unsetConfig(ctx, account, unsetConfigInput{Name: "hello", Keys: []string{"ONE"}}); err != nil {
		t.Fatalf("unset_config: %v", err)
	}

	want := map[string]string{
		"app_1/ONE":     auth.ActionConfigUnset,
		"app_1/API_KEY": auth.ActionConfigSet,
	}
	seen := map[string]bool{}
	for _, row := range auditor.rows {
		if row.TargetType != auth.TargetAppConfig {
			continue
		}
		seen[row.TargetID+" "+row.Action] = true
		// The app id and the key both survive in the one pair the table has.
		if !strings.HasPrefix(row.TargetID, "app_1/") {
			t.Errorf("audit row targets %q, want the app id and the key joined", row.TargetID)
		}
		for _, value := range []string{"hunter2", "1"} {
			if strings.Contains(row.TargetID, "="+value) || row.Reason == value {
				t.Errorf("audit row %+v carries a value", row)
			}
		}
	}
	if !seen["app_1/ONE "+auth.ActionConfigSet] {
		t.Error("setting ONE left no audit row")
	}
	for target, action := range want {
		if !seen[target+" "+action] {
			t.Errorf("no audit row for %s under %s", target, action)
		}
	}
}

func TestTheBoundsCountWhatTheAppAlreadyHolds(t *testing.T) {
	// covers: AC-6
	// The domain rule is tested on its own, but only this path proves the tool
	// reads the app's current keys and hands them to it. Without that read both
	// bounds would silently become per call, and a caller could hold any number
	// of keys by sending them a few at a time.
	cases := map[string]struct {
		seed   map[string]configValue
		then   map[string]configValue
		reason domain.Reason
	}{
		"one key past the count bound": {
			seed:   nKeys(domain.MaxConfigKeys, "v"),
			then:   map[string]configValue{"ONE_MORE": entry("v", false)},
			reason: domain.ReasonConfigTooManyKeys,
		},
		"one value past the total size bound": {
			// Each value is under the single value bound, so the only thing that can
			// refuse this is the total taken over the merge.
			seed:   nKeys(7, strings.Repeat("x", domain.MaxConfigValueBytes)),
			then:   map[string]configValue{"LAST": entry(strings.Repeat("x", domain.MaxConfigValueBytes), false)},
			reason: domain.ReasonConfigTooLarge,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, account := configServer()
			ctx := t.Context()
			if _, _, err := s.setConfig(ctx, account, setConfigInput{Name: "hello", Config: tc.seed}); err != nil {
				t.Fatalf("seeding %d keys: %v", len(tc.seed), err)
			}

			_, _, err := s.setConfig(ctx, account, setConfigInput{Name: "hello", Config: tc.then})
			if err == nil || !strings.HasPrefix(err.Error(), string(tc.reason)) {
				t.Fatalf("the call answered %v, want %s", err, tc.reason)
			}
			_, out, err := s.getConfig(ctx, account, getConfigInput{Name: "hello"})
			if err != nil {
				t.Fatalf("get_config: %v", err)
			}
			if len(out.Config) != len(tc.seed) {
				t.Errorf("the refused call left %d keys, want the %d seeded", len(out.Config), len(tc.seed))
			}
		})
	}
}

func TestARefusedChangeIsAuditedWithItsReasonAndNoValue(t *testing.T) {
	// covers: AC-12
	// A refusal is an access decision, so it is recorded like one. The row names
	// the app and the reason code, and the value the caller sent is not part of
	// either.
	s, auditor, account := configServer()
	ctx := t.Context()

	_, _, err := s.setConfig(ctx, account, setConfigInput{
		Name:   "hello",
		Config: map[string]configValue{"PORT": entry("hunter2", false)},
	})
	if err == nil {
		t.Fatal("the reserved key was accepted")
	}

	var found bool
	for _, row := range auditor.rows {
		if row.Reason != string(domain.ReasonConfigKeyReserved) {
			continue
		}
		found = true
		if row.Action != auth.ActionConfigSet || row.TargetID != "app_1" {
			t.Errorf("the refusal is recorded as %+v, want it against the app under %s", row, auth.ActionConfigSet)
		}
		if strings.Contains(row.TargetID+row.Reason, "hunter2") {
			t.Errorf("the refusal row carries the value: %+v", row)
		}
	}
	if !found {
		t.Errorf("the refusal left no audit row: %+v", auditor.rows)
	}
}

func TestUnsetConfigRefusesAKeyThatCouldNeverHaveBeenStored(t *testing.T) {
	// covers: AC-3, AC-4
	// A badly shaped key is not the same answer as a key that is simply not set.
	// Without the shape check here it would fall through to the store, come back
	// as a missing row, and the caller would be told config_key_unknown about a
	// key no app could ever hold.
	s, _, account := configServer()
	ctx := t.Context()
	if _, _, err := s.setConfig(ctx, account, setConfigInput{
		Name: "hello", Config: map[string]configValue{"KEEP": entry("me", false)},
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for _, keys := range [][]string{{"bad key"}, {"KEEP", "lower"}, nil} {
		_, _, err := s.unsetConfig(ctx, account, unsetConfigInput{Name: "hello", Keys: keys})
		if err == nil || !strings.HasPrefix(err.Error(), string(domain.ReasonConfigKeyInvalid)) {
			t.Errorf("unsetting %v answered %v, want %s", keys, err, domain.ReasonConfigKeyInvalid)
		}
	}
	_, out, err := s.getConfig(ctx, account, getConfigInput{Name: "hello"})
	if err != nil {
		t.Fatalf("get_config: %v", err)
	}
	if len(out.Config) != 1 {
		t.Errorf("a refused unset changed the configuration: %+v", out.Config)
	}
}

// nKeys is n distinct valid keys, all holding the same value.
func nKeys(n int, value string) map[string]configValue {
	config := make(map[string]configValue, n)
	for i := range n {
		config["K"+strings.Repeat("A", i/26)+string(rune('A'+i%26))] = entry(value, false)
	}
	return config
}

func TestGetConfigOnAnAppWithNothingSetIsAnEmptyList(t *testing.T) {
	s, auditor, account := configServer()
	_, out, err := s.getConfig(t.Context(), account, getConfigInput{Name: "hello"})
	if err != nil {
		t.Fatalf("get_config: %v", err)
	}
	if len(out.Config) != 0 || out.AppName != "hello" {
		t.Errorf("output = %+v, want an empty list for hello", out)
	}
	// A read that succeeded is not an access decision, so it is not recorded.
	if len(auditor.rows) != 0 {
		t.Errorf("a successful read recorded %+v", auditor.rows)
	}
}
