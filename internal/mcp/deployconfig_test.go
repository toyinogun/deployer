package mcp

import (
	"strings"
	"testing"

	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
)

func TestDeployAppSetsTheConfigurationItIsGiven(t *testing.T) {
	// covers: spec 0010 AC-9
	account := auth.Account{ID: "acc_1"}
	apps := &stubApps{existing: map[string]App{"hello": {ID: "app_1", Slug: "hello-a1b2c3", Name: "hello"}}}
	deployments := &stubDeployments{}
	s, _ := server(apps, deployments, liveUpload(account.ID))

	if _, _, err := s.deploy(t.Context(), account, deployInput{
		Name: "hello", UploadID: "upl_1",
		Config: map[string]configValue{
			"DATABASE_URL": entry("postgres://db/app", true),
			"LOG_LEVEL":    entry("debug", false),
		},
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	_, out, err := s.getConfig(t.Context(), account, getConfigInput{Name: "hello"})
	if err != nil {
		t.Fatalf("get_config: %v", err)
	}
	if len(out.Config) != 2 {
		t.Fatalf("after the deploy the app holds %+v, want both keys", out.Config)
	}
	secret, _ := valueOf(out, "DATABASE_URL")
	if !secret.Secret || secret.Value != nil {
		t.Errorf("the secret set through deploy_app came back as %+v", secret)
	}
	if deployments.created != 1 {
		t.Errorf("deployments created = %d, want 1", deployments.created)
	}
}

func TestDeployAppWithNoConfigMapLeavesTheConfigurationAlone(t *testing.T) {
	// covers: spec 0010 AC-9
	account := auth.Account{ID: "acc_1"}
	apps := &stubApps{existing: map[string]App{"hello": {ID: "app_1", Slug: "hello-a1b2c3", Name: "hello"}}}
	s, _ := server(apps, &stubDeployments{}, liveUpload(account.ID))
	ctx := t.Context()
	if _, _, err := s.setConfig(ctx, account, setConfigInput{
		Name: "hello", Config: map[string]configValue{"KEEP": entry("me", false)},
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if _, _, err := s.deploy(ctx, account, deployInput{Name: "hello", UploadID: "upl_1"}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	_, out, err := s.getConfig(ctx, account, getConfigInput{Name: "hello"})
	if err != nil {
		t.Fatalf("get_config: %v", err)
	}
	got, ok := valueOf(out, "KEEP")
	if len(out.Config) != 1 || !ok || got.Value == nil || *got.Value != "me" {
		t.Errorf("a deploy with no config map changed the configuration: %+v", out.Config)
	}
}

func TestDeployAppEnforcesTheSameConfigurationRulesAsSetConfig(t *testing.T) {
	// covers: spec 0010 AC-9
	cases := map[string]struct {
		config map[string]configValue
		reason domain.Reason
	}{
		"a reserved key":   {map[string]configValue{"PORT": entry("9000", false)}, domain.ReasonConfigKeyReserved},
		"a bad key":        {map[string]configValue{"bad key": entry("1", false)}, domain.ReasonConfigKeyInvalid},
		"a missing flag":   {map[string]configValue{"TOKEN": {Value: "abc"}}, domain.ReasonConfigFlagMissing},
		"an oversized one": {map[string]configValue{"BIG": entry(strings.Repeat("x", domain.MaxConfigValueBytes+1), false)}, domain.ReasonConfigTooLarge},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			account := auth.Account{ID: "acc_1"}
			apps := &stubApps{existing: map[string]App{"hello": {ID: "app_1", Slug: "hello-a1b2c3", Name: "hello"}}}
			deployments := &stubDeployments{}
			s, _ := server(apps, deployments, liveUpload(account.ID))

			_, _, err := s.deploy(t.Context(), account, deployInput{
				Name: "hello", UploadID: "upl_1", Config: tc.config,
			})
			if err == nil || !strings.HasPrefix(err.Error(), string(tc.reason)) {
				t.Fatalf("deploy answered %v, want %s", err, tc.reason)
			}
			// A refused configuration starts no deployment either.
			if deployments.created != 0 {
				t.Errorf("a refused config map still created %d deployments", deployments.created)
			}
			if len(apps.config["app_1"]) != 0 {
				t.Errorf("a refused config map wrote %+v", apps.config["app_1"])
			}
		})
	}
}

func TestTheDeployDescriptionCarriesTheConfigContract(t *testing.T) {
	// covers: spec 0010 AC-5, AC-9
	// The description is contract rather than decoration, and nothing else tests
	// that it kept up with the arguments the tool actually takes.
	s, _ := server(&stubApps{}, &stubDeployments{}, Upload{})
	description := s.toolDescription()
	for _, must := range []string{"config", "secret flag", "PORT", "APP_URL"} {
		if !strings.Contains(description, must) {
			t.Errorf("deploy_app's description does not mention %q", must)
		}
	}
}
