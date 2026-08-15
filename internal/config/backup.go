package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
)

// The defaults for the two optional backup values.
const (
	defaultBackupInterval = 24 * time.Hour
	defaultBackupRegion   = "auto"
)

// backupGroup is the all or nothing set. Present them all and backups are on,
// present none and backups are off, present some and the process refuses to
// start. The alert email is inside the group deliberately: backups configured
// with nowhere to complain to is the precise failure this feature exists to
// prevent (spec 0020, AC-16).
var backupGroup = []string{
	"DEPLOYER_BACKUP_AGE_RECIPIENT",
	"DEPLOYER_BACKUP_S3_ENDPOINT",
	"DEPLOYER_BACKUP_S3_BUCKET",
	"DEPLOYER_BACKUP_S3_ACCESS_KEY_ID",
	"DEPLOYER_BACKUP_S3_SECRET_ACCESS_KEY",
	"DEPLOYER_BACKUP_ALERT_EMAIL",
}

// loadBackup reads the settings spec 0020 adds. The two optional values are
// validated whether or not the group is present, because a bad number is a bad
// number either way. The recipient is parsed here, once, rather than at first
// use: a typo in it would otherwise be discovered by the first backup run, which
// is the one moment nobody is watching (AC-16).
func loadBackup(getenv func(string) string, c *Config) (errs []string) {
	c.BackupInterval = defaultBackupInterval
	if raw := getenv("DEPLOYER_BACKUP_INTERVAL_SECONDS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			errs = append(errs,
				fmt.Sprintf("DEPLOYER_BACKUP_INTERVAL_SECONDS must be a positive whole number of seconds, got %q", raw))
		} else {
			c.BackupInterval = time.Duration(n) * time.Second
		}
	}

	c.BackupS3Region = defaultBackupRegion
	if raw := getenv("DEPLOYER_BACKUP_S3_REGION"); raw != "" {
		c.BackupS3Region = raw
	}

	var present, absent []string
	for _, key := range backupGroup {
		if strings.TrimSpace(getenv(key)) == "" {
			absent = append(absent, key)
		} else {
			present = append(present, key)
		}
	}
	switch {
	case len(present) == 0:
		// Backups are off. The platform boots, warns once at startup naming what
		// is missing, takes no backups, records no runs, and works in every
		// other respect, exactly as the Resend key and the bootstrap token
		// already behave (AC-15).
		return errs
	case len(absent) > 0:
		sort.Strings(absent)
		errs = append(errs, fmt.Sprintf(
			"backups are partly configured: %s are set but %s are not. "+
				"All six or none, because a backup with nowhere to report a failure is the thing this exists to prevent",
			strings.Join(present, ", "), strings.Join(absent, ", ")))
		return errs
	}

	c.BackupAgeRecipientRaw = getenv("DEPLOYER_BACKUP_AGE_RECIPIENT")
	recipient, err := age.ParseX25519Recipient(c.BackupAgeRecipientRaw)
	if err != nil {
		// The value itself is not echoed. It is a public key rather than a
		// secret, but a config error is not the place to start deciding which
		// values are safe to print.
		errs = append(errs, "DEPLOYER_BACKUP_AGE_RECIPIENT is not an age public recipient (it should start with age1)")
	} else {
		c.BackupAgeRecipient = recipient
	}

	c.BackupS3Endpoint = getenv("DEPLOYER_BACKUP_S3_ENDPOINT")
	if !strings.HasPrefix(c.BackupS3Endpoint, "http://") && !strings.HasPrefix(c.BackupS3Endpoint, "https://") {
		errs = append(errs, fmt.Sprintf(
			"DEPLOYER_BACKUP_S3_ENDPOINT must be an absolute http or https address, got %q", c.BackupS3Endpoint))
	}
	c.BackupS3Bucket = getenv("DEPLOYER_BACKUP_S3_BUCKET")
	c.BackupS3AccessKeyID = getenv("DEPLOYER_BACKUP_S3_ACCESS_KEY_ID")
	c.BackupS3SecretAccessKey = getenv("DEPLOYER_BACKUP_S3_SECRET_ACCESS_KEY")
	c.BackupAlertEmail = getenv("DEPLOYER_BACKUP_ALERT_EMAIL")
	if !strings.Contains(c.BackupAlertEmail, "@") {
		errs = append(errs, "DEPLOYER_BACKUP_ALERT_EMAIL must be an email address")
	}
	return errs
}

// BackupsConfigured reports whether the all or nothing group was present. It is
// what the wiring reads to decide whether to build a backup service at all.
func (c Config) BackupsConfigured() bool { return c.BackupAgeRecipient != nil }

// MissingBackupValues lists the group members that are unset, for the one
// startup warning an unconfigured platform logs (AC-15).
func (c Config) MissingBackupValues(getenv func(string) string) []string {
	var absent []string
	for _, key := range backupGroup {
		if strings.TrimSpace(getenv(key)) == "" {
			absent = append(absent, key)
		}
	}
	return absent
}
