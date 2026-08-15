package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/toyinogun/deployer/internal/backup"
)

// restore is the `deployer restore` subcommand, intended to run outside the
// cluster on a machine holding the age identity the cluster never does. It loads
// none of the control plane's configuration and opens no database: it takes the
// bucket from flags or the same environment names the pod uses, and the identity
// from a file (spec 0020, AC-23).
func restore(ctx context.Context, args []string, getenv func(string) string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	key := fs.String("key", "", "the object key to restore, as shown on the admin backups page")
	// The identity is a file path and only ever a file path. An environment
	// variable or an argument would put a private key in a process listing
	// (AC-23).
	identity := fs.String("identity", "", "path to the age identity file that decrypts the backup")
	out := fs.String("out", "", "where to write the restored database; it must not already exist")
	endpoint := fs.String("endpoint", getenv("DEPLOYER_BACKUP_S3_ENDPOINT"), "the S3 compatible endpoint")
	bucket := fs.String("bucket", getenv("DEPLOYER_BACKUP_S3_BUCKET"), "the bucket the backups are in")
	region := fs.String("region", getenv("DEPLOYER_BACKUP_S3_REGION"), "the bucket region, auto for R2")
	fs.Usage = func() {
		// A failed write to the usage stream is not worth a second failure path
		// in a command whose whole job is the restore below.
		_, _ = fmt.Fprint(fs.Output(),
			"usage: deployer restore -key <object key> -identity <age key file> -out <path>\n\n"+
				"The bucket credential is read from DEPLOYER_BACKUP_S3_ACCESS_KEY_ID and\n"+
				"DEPLOYER_BACKUP_S3_SECRET_ACCESS_KEY. The identity is never read from the\n"+
				"environment: it is a file, so it stays out of a process listing.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, err := backup.NewS3Store(backup.S3Options{
		Endpoint:        *endpoint,
		Bucket:          *bucket,
		Region:          *region,
		AccessKeyID:     getenv("DEPLOYER_BACKUP_S3_ACCESS_KEY_ID"),
		SecretAccessKey: getenv("DEPLOYER_BACKUP_S3_SECRET_ACCESS_KEY"),
	})
	if err != nil {
		return err
	}

	if err := backup.Restore(ctx, store, backup.RestoreOptions{
		Key: *key, IdentityPath: *identity, Out: *out,
	}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "restored %s to %s\n"+
		"deploy/README.md has the rest: scale the control plane down, place the file, scale back up.\n",
		*key, *out)
	return nil
}
