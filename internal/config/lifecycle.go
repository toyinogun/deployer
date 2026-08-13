package config

import (
	"fmt"
	"strconv"
	"time"
)

// The orphan reaper's two knobs, added by spec 0012. Both are validated here at
// startup like every other DEPLOYER_* variable, rather than discovered to be
// nonsense by the first pass.
//
// Neither has any required relationship to DEPLOYER_DEPLOY_TIMEOUT_SECONDS. An
// app's row is live for the whole of every build, so an app mid deploy is never
// a reaper candidate whatever these are set to; the grace guards only the window
// between a namespace being created and the reaper reading the slug list
// (spec 0012, AC-26, Key invariants).
const (
	defaultReapInterval = 600 * time.Second
	defaultOrphanGrace  = 900 * time.Second
)

// loadLifecycle reads the reaper's cadence and grace, reporting anything that is
// not a positive number of seconds.
func loadLifecycle(getenv func(string) string, c *Config) (errs []string) {
	c.ReapInterval = defaultReapInterval
	c.OrphanGrace = defaultOrphanGrace
	for _, v := range []struct {
		key  string
		into *time.Duration
	}{
		{"DEPLOYER_REAP_INTERVAL_SECONDS", &c.ReapInterval},
		{"DEPLOYER_ORPHAN_GRACE_SECONDS", &c.OrphanGrace},
	} {
		raw := getenv(v.key)
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Sprintf("%s must be a positive integer number of seconds, got %q", v.key, raw))
			continue
		}
		*v.into = time.Duration(n) * time.Second
	}
	return errs
}
