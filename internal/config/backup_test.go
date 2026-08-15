package config

import (
	"strings"
	"testing"
	"time"
)

// backupSix is the all or nothing group with a value for each, the shape a
// platform that takes backups is configured in.
var backupSix = map[string]string{
	"DEPLOYER_BACKUP_AGE_RECIPIENT":        "age1j3svt4hfzlqq75eqmsyc0a7rvlvxjd0ddh3ckn82p0u0852w7azqj2uuuw",
	"DEPLOYER_BACKUP_S3_ENDPOINT":          "https://example.r2.cloudflarestorage.com",
	"DEPLOYER_BACKUP_S3_BUCKET":            "deployer-backups",
	"DEPLOYER_BACKUP_S3_ACCESS_KEY_ID":     "an-access-key",
	"DEPLOYER_BACKUP_S3_SECRET_ACCESS_KEY": "a-secret-key",
	"DEPLOYER_BACKUP_ALERT_EMAIL":          "someone@example.test",
}

// TestAnUnconfiguredPlatformBootsWithBackupsOff is AC-15: no bucket configured
// means the platform boots and works in every other respect.
// covers: AC-15
func TestAnUnconfiguredPlatformBootsWithBackupsOff(t *testing.T) {
	c, err := Load(env(valid))
	if err != nil {
		t.Fatalf("an unconfigured platform should boot: %v", err)
	}
	if c.BackupsConfigured() {
		t.Error("backups read as configured with nothing set")
	}
	if missing := c.MissingBackupValues(env(valid)); len(missing) != 6 {
		t.Errorf("the startup warning should name all six missing values, names %v", missing)
	}
}

// TestAllSixBackupValuesTurnBackupsOn is AC-16: present them all and backups are
// on, with the recipient parsed here rather than at first use.
// covers: AC-16
func TestAllSixBackupValuesTurnBackupsOn(t *testing.T) {
	c, err := Load(env(withValid(backupSix)))
	if err != nil {
		t.Fatalf("a fully configured platform should boot: %v", err)
	}
	if !c.BackupsConfigured() {
		t.Fatal("backups should be on with all six values set")
	}
	if c.BackupAgeRecipient == nil {
		t.Error("the recipient should be parsed at startup, not at first use")
	}
	if c.BackupS3Region != "auto" {
		t.Errorf("the region defaults to %q, want auto", c.BackupS3Region)
	}
	if c.BackupInterval != 24*time.Hour {
		t.Errorf("the interval defaults to %s, want a day", c.BackupInterval)
	}
}

// TestAPartlyConfiguredPlatformRefusesToStart is AC-16, one case per member of
// the group, so no single value can be quietly optional. The alert email is in
// here deliberately: backups with nowhere to complain to is the failure this
// feature exists to prevent.
// covers: AC-16
func TestAPartlyConfiguredPlatformRefusesToStart(t *testing.T) {
	for absent := range backupSix {
		partial := map[string]string{}
		for k, v := range backupSix {
			if k != absent {
				partial[k] = v
			}
		}
		_, err := Load(env(withValid(partial)))
		if err == nil {
			t.Errorf("a platform missing only %s started, want the boot refused", absent)
			continue
		}
		if !strings.Contains(err.Error(), absent) {
			t.Errorf("the error for a missing %s reads %q, want it to name the variable", absent, err)
		}
	}
}

// TestTheBackupValuesAreValidatedAtStartup keeps a bad value from being
// discovered by the first backup run, which is the one moment nobody is watching.
// covers: AC-16
func TestTheBackupValuesAreValidatedAtStartup(t *testing.T) {
	for name, override := range map[string]map[string]string{
		"a recipient that is not an age key": {"DEPLOYER_BACKUP_AGE_RECIPIENT": "not-an-age-key"},
		"an endpoint with no scheme":         {"DEPLOYER_BACKUP_S3_ENDPOINT": "example.r2.cloudflarestorage.com"},
		"an alert address that is not one":   {"DEPLOYER_BACKUP_ALERT_EMAIL": "nobody"},
	} {
		with := map[string]string{}
		for k, v := range backupSix {
			with[k] = v
		}
		for k, v := range override {
			with[k] = v
		}
		if _, err := Load(env(withValid(with))); err == nil {
			t.Errorf("%s was accepted, want the boot refused", name)
		}
	}

	// The interval is optional, and a nonsense one is still refused rather than
	// silently defaulted.
	for _, bad := range []string{"0", "-1", "daily"} {
		with := map[string]string{"DEPLOYER_BACKUP_INTERVAL_SECONDS": bad}
		if _, err := Load(env(withValid(with))); err == nil {
			t.Errorf("DEPLOYER_BACKUP_INTERVAL_SECONDS=%q was accepted, want the boot refused", bad)
		}
	}
}

// TestTheBackupIntervalTakesAnOverride pins that a value in seconds is read as
// one, and that it is validated whether or not the group is present.
// covers: AC-16
func TestTheBackupIntervalTakesAnOverride(t *testing.T) {
	c, err := Load(env(withValid(map[string]string{"DEPLOYER_BACKUP_INTERVAL_SECONDS": "3600"})))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.BackupInterval != time.Hour {
		t.Errorf("the interval reads %s, want an hour", c.BackupInterval)
	}
	if c.BackupsConfigured() {
		t.Error("an interval on its own must not turn backups on")
	}
}

// TestTheAgeIdentityHasNoConfigurationValue is the invariant the whole design
// rests on: nothing gives the cluster the private half.
// covers: AC-4
func TestTheAgeIdentityHasNoConfigurationValue(t *testing.T) {
	// A variable somebody might reach for, proven to be read by nothing.
	read := false
	getenv := func(key string) string {
		if strings.Contains(key, "IDENTITY") || strings.Contains(key, "PRIVATE") {
			read = true
		}
		return env(withValid(backupSix))(key)
	}
	if _, err := Load(getenv); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if read {
		t.Error("configuration read a variable that could hold an age identity")
	}
}
