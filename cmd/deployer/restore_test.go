package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bucketEnv is a getenv that answers the bucket variables the pod uses, so the
// command gets far enough to reach the restore itself.
func bucketEnv(asked *[]string) func(string) string {
	values := map[string]string{
		"DEPLOYER_BACKUP_S3_ENDPOINT":          "https://abc123.r2.cloudflarestorage.com",
		"DEPLOYER_BACKUP_S3_BUCKET":            "deployer-backups",
		"DEPLOYER_BACKUP_S3_REGION":            "auto",
		"DEPLOYER_BACKUP_S3_ACCESS_KEY_ID":     "an-access-key",
		"DEPLOYER_BACKUP_S3_SECRET_ACCESS_KEY": "a-secret-key",
	}
	return func(k string) string {
		if asked != nil {
			*asked = append(*asked, k)
		}
		return values[k]
	}
}

// covers: AC-23 - the identity is a file path and only ever a file path. An
// environment variable or an argument would put a private key in a process
// listing, so the command must never go looking for one in the environment.
func TestRestore_neverReadsAnIdentityFromTheEnvironment(t *testing.T) {
	var asked []string
	env := bucketEnv(&asked)

	// The error does not matter here; what matters is what was asked for on the
	// way to it.
	_ = restore(t.Context(), []string{
		"-key", "db/20260815T030000Z-bkp_a.age",
		"-identity", filepath.Join(t.TempDir(), "identity.txt"),
		"-out", filepath.Join(t.TempDir(), "restored.db"),
	}, env)

	for _, name := range asked {
		upper := strings.ToUpper(name)
		if strings.Contains(upper, "IDENTITY") || strings.Contains(upper, "AGE") || strings.Contains(upper, "PRIVATE") {
			t.Errorf("the command asked the environment for %q; the identity is a file, never a variable", name)
		}
	}
	if len(asked) == 0 {
		t.Fatal("the command should still read the bucket variables from the environment")
	}
}

// covers: AC-23 - the bucket comes from the same variable names the pod uses, so
// a restore on your own machine needs the credential exported and nothing else.
func TestRestore_takesTheBucketFromTheSameVariablesThePodUses(t *testing.T) {
	var asked []string

	_ = restore(t.Context(), []string{
		"-key", "db/object.age",
		"-identity", filepath.Join(t.TempDir(), "identity.txt"),
		"-out", filepath.Join(t.TempDir(), "restored.db"),
	}, bucketEnv(&asked))

	want := []string{
		"DEPLOYER_BACKUP_S3_ENDPOINT",
		"DEPLOYER_BACKUP_S3_BUCKET",
		"DEPLOYER_BACKUP_S3_ACCESS_KEY_ID",
		"DEPLOYER_BACKUP_S3_SECRET_ACCESS_KEY",
	}
	for _, name := range want {
		found := false
		for _, got := range asked {
			if got == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the command never read %s", name)
		}
	}
}

// An endpoint that is not configured is refused where the client is built,
// rather than producing a confusing failure on the first request.
func TestRestore_refusesAnUnconfiguredEndpoint(t *testing.T) {
	err := restore(t.Context(), []string{
		"-key", "db/object.age",
		"-identity", filepath.Join(t.TempDir(), "identity.txt"),
		"-out", filepath.Join(t.TempDir(), "restored.db"),
	}, func(string) string { return "" })

	if err == nil {
		t.Fatal("want a refusal with no endpoint configured")
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("the refusal should name the endpoint's shape, got %v", err)
	}
}

// covers: AC-23, AC-24 - the three arguments the restore needs are surfaced by
// the command, not swallowed on the way through.
func TestRestore_refusesAnIncompleteCommandLine(t *testing.T) {
	identity := filepath.Join(t.TempDir(), "identity.txt")
	out := filepath.Join(t.TempDir(), "restored.db")

	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "no key", args: []string{"-identity", identity, "-out", out}},
		{name: "no identity", args: []string{"-key", "db/object.age", "-out", out}},
		{name: "no destination", args: []string{"-key", "db/object.age", "-identity", identity}},
		{name: "no arguments at all", args: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := restore(t.Context(), tt.args, bucketEnv(nil))

			if err == nil {
				t.Fatal("want a refusal")
			}
			if !strings.Contains(err.Error(), "-key") {
				t.Errorf("the refusal should name what the command needs, got %v", err)
			}
		})
	}
}

// covers: AC-24 - the destination check reaches the command line, so a mistyped
// -out is refused before anything is downloaded.
func TestRestore_refusesADestinationThatExists(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "restored.db")
	if err := os.WriteFile(out, []byte("the database that was already there"), 0o600); err != nil {
		t.Fatalf("placing a file at the destination: %v", err)
	}

	err := restore(t.Context(), []string{
		"-key", "db/object.age",
		"-identity", filepath.Join(dir, "identity.txt"),
		"-out", out,
	}, bucketEnv(nil))

	if err == nil {
		t.Fatal("want a refusal when the destination exists")
	}
	if !strings.Contains(err.Error(), "never overwrites") {
		t.Errorf("the refusal should say why, got %v", err)
	}
}

// An argument the command does not have is a parse failure, not a silently
// ignored flag on a command that is about to write a database.
func TestRestore_refusesAnUnknownFlag(t *testing.T) {
	err := restore(t.Context(), []string{"-force", "-key", "db/object.age"}, bucketEnv(nil))

	if err == nil {
		t.Fatal("want a parse failure for a flag the command does not have")
	}
}
