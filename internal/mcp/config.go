package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/toyinogun/deployer/internal/auth"
	"github.com/toyinogun/deployer/internal/domain"
)

// ErrNoConfigKey means an unset named a key the app does not have. The whole
// call is refused and nothing is removed (spec 0010, AC-3).
var ErrNoConfigKey = errors.New("mcp: no such configuration key")

// The three tool descriptions. Like every description here they are contract
// rather than decoration: they are where an agent learns that a change waits for
// the next deploy, that a secret value never comes back, and that the flag is
// required on every key (spec 0010, AC-2, AC-8, AC-16).
const (
	setConfigDescription = `Set environment variables for an app, which it receives on its next deploy.

Give the app's name and a config map. Each entry needs a value and a secret
flag, and the flag is required every time: sending a key without it is refused
rather than defaulted, so an existing secret can never quietly become a plain
value.

Mark anything credential shaped as secret. A secret value is never returned by
any tool, including this one, and is blanked out of get_logs.

Keys must look like environment variable names: upper case letters, digits, and
underscores, not starting with a digit. PORT and APP_URL are set by the platform
and cannot be configured here.

Keys already set and not named here are left alone. An empty string is a real
value, which is not the same as the key being unset.

Nothing about the running app changes. The values reach the container the next
time deploy_app runs, and the response says so.

An app may hold at most 64 keys, a value at most 4 KB, and the whole
configuration at most 32 KB. A call that breaks any rule is refused whole:
either every key in it is written or none is.`

	unsetConfigDescription = `Remove environment variables from an app, taking effect on its next deploy.

Give the app's name and the keys to remove. If any of them is not set, the whole
call is refused and nothing is removed, so a typo cannot half apply.

The running app is untouched. The keys are gone from the container the next time
deploy_app runs.`

	getConfigDescription = `List the environment variables set for an app.

Every key comes back with its secret flag. A secret key's value is null, always,
which is the point of marking it secret: use this to check what is set, not to
read a credential back.

PORT and APP_URL are not listed here. The platform sets them on every app.`
)

// ConfigEntry is one stored configuration key. Value is empty for a secret key
// on the response path, and carries the real value on the deploy path: which one
// a caller gets depends on which store method produced it, never on a flag read
// in this package (spec 0010, AC-2).
type ConfigEntry struct {
	Key    string
	Value  string
	Secret bool
}

// configValue is one entry of a caller's config map. Secret is a pointer so an
// omitted flag is a refusal rather than a default (AC-16).
//
// The json tag carries omitempty so the generated schema does not mark the flag
// required. That reads backwards, because the flag is required: it is what keeps
// the refusal ours. A field the schema marks required is rejected by the sdk
// before any handler runs, so the caller is handed a validation string instead
// of config_flag_missing and the refusal is never audited. Left optional in the
// schema, ValidateConfig decides it, which is where every other configuration
// rule is decided.
type configValue struct {
	Value  string `json:"value" jsonschema:"the value the app receives, which may be an empty string"`
	Secret *bool  `json:"secret,omitempty" jsonschema:"required on every key, and a key sent without it is refused with config_flag_missing; true keeps the value out of every response and blanks it from the logs"`
}

// setConfigInput is set_config's whole argument surface.
type setConfigInput struct {
	Name   string                 `json:"name" jsonschema:"the app's name, the same one deploy_app was given"`
	Config map[string]configValue `json:"config" jsonschema:"the keys to set, each with a value and a required secret flag"`
}

// unsetConfigInput is unset_config's whole argument surface.
type unsetConfigInput struct {
	Name string   `json:"name" jsonschema:"the app's name, the same one deploy_app was given"`
	Keys []string `json:"keys" jsonschema:"the keys to remove; if any of them is not set the whole call is refused"`
}

// getConfigInput is get_config's whole argument surface.
type getConfigInput struct {
	Name string `json:"name" jsonschema:"the app's name, the same one deploy_app was given"`
}

// configOutput is what all three tools answer with. The value is a pointer so a
// secret key is null rather than an empty string, which an empty value would
// otherwise be indistinguishable from (AC-2, AC-15).
type configOutput struct {
	AppName             string           `json:"app_name"`
	Config              []configEntryOut `json:"config"`
	AppliesOnNextDeploy bool             `json:"applies_on_next_deploy,omitempty"`
	Note                string           `json:"note,omitempty"`
}

type configEntryOut struct {
	Key    string  `json:"key"`
	Secret bool    `json:"secret"`
	Value  *string `json:"value"`
}

// noteNextDeploy is the sentence a write carries. A change that does nothing
// until the next deploy has to say so, because the response is the only place
// the caller finds out (AC-8).
const noteNextDeploy = "the app keeps running with its current values; these reach the container on its next deploy"

// setConfig writes the given keys, merging them with whatever is already set.
func (s *Server) setConfig(ctx context.Context, account auth.Account, in setConfigInput) (*mcp.CallToolResult, configOutput, error) {
	app, err := s.resolveOwned(ctx, account, in.Name, auth.ActionConfigSet)
	if err != nil {
		return nil, configOutput{}, err
	}
	if reason := s.writeConfig(ctx, account, app, in.Config, auth.ActionConfigSet); reason != "" {
		return nil, configOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionConfigSet, reason, nil)
	}
	return s.configResponse(ctx, app, auth.ActionConfigSet, true)
}

// unsetConfig removes the given keys, or none of them.
func (s *Server) unsetConfig(ctx context.Context, account auth.Account, in unsetConfigInput) (*mcp.CallToolResult, configOutput, error) {
	app, err := s.resolveOwned(ctx, account, in.Name, auth.ActionConfigUnset)
	if err != nil {
		return nil, configOutput{}, err
	}
	if reason := domain.ValidateUnset(in.Keys); reason != "" {
		return nil, configOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionConfigUnset, reason, nil)
	}
	err = s.apps.UnsetConfig(ctx, app.ID, in.Keys)
	if errors.Is(err, ErrNoConfigKey) {
		return nil, configOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionConfigUnset,
			domain.ReasonConfigKeyUnknown, err)
	}
	if err != nil {
		return nil, configOutput{}, toolError(auth.ActionConfigUnset, domain.ReasonInternal,
			fmt.Errorf("unsetting configuration on app %s: %w", app.ID, err))
	}
	for _, key := range in.Keys {
		s.recordConfigChange(ctx, account.ID, app.ID, key, auth.ActionConfigUnset)
	}
	return s.configResponse(ctx, app, auth.ActionConfigUnset, true)
}

// getConfig lists what is set, with every secret value withheld.
func (s *Server) getConfig(ctx context.Context, account auth.Account, in getConfigInput) (*mcp.CallToolResult, configOutput, error) {
	app, err := s.resolveOwned(ctx, account, in.Name, auth.ActionConfigGet)
	if err != nil {
		return nil, configOutput{}, err
	}
	return s.configResponse(ctx, app, auth.ActionConfigGet, false)
}

// writeConfig validates a whole config map and writes it, and is the one path
// both set_config and deploy_app's optional map go through, so the two can never
// enforce different rules (AC-9). It returns the reason the call is refused
// with, or the empty Reason once the write has landed.
func (s *Server) writeConfig(ctx context.Context, account auth.Account, app App, config map[string]configValue, action string) domain.Reason {
	writes := make([]domain.ConfigWrite, 0, len(config))
	for _, key := range sortedKeys(config) {
		writes = append(writes, domain.ConfigWrite{
			Key:    key,
			Value:  config[key].Value,
			Secret: config[key].Secret,
		})
	}

	// The bounds are on the merged result, so what the app already holds is part
	// of the question. The values read here never leave the process.
	current, err := s.apps.ConfigValues(ctx, app.ID)
	if err != nil {
		return domain.ReasonInternal
	}
	existing := make(map[string]string, len(current))
	for _, e := range current {
		existing[e.Key] = e.Value
	}
	if reason := domain.ValidateConfig(writes, existing); reason != "" {
		return reason
	}

	entries := make([]ConfigEntry, 0, len(writes))
	for _, w := range writes {
		entries = append(entries, ConfigEntry{Key: w.Key, Value: w.Value, Secret: w.IsSecret()})
	}
	if err := s.apps.SetConfig(ctx, app.ID, entries); err != nil {
		return domain.ReasonInternal
	}
	// One row per key, after the write, because an audit row for a write that did
	// not happen is worse than none (AC-12).
	for _, w := range writes {
		s.recordConfigChange(ctx, account.ID, app.ID, w.Key, action)
	}
	return ""
}

// configResponse reads the configuration back through the one path that withholds
// secret values, so no code here decides what a caller may see (AC-2).
func (s *Server) configResponse(ctx context.Context, app App, action string, wrote bool) (*mcp.CallToolResult, configOutput, error) {
	entries, err := s.apps.Config(ctx, app.ID)
	if err != nil {
		return nil, configOutput{}, toolError(action, domain.ReasonInternal,
			fmt.Errorf("reading configuration for app %s: %w", app.ID, err))
	}
	out := configOutput{AppName: app.Name, Config: make([]configEntryOut, 0, len(entries))}
	for _, e := range entries {
		entry := configEntryOut{Key: e.Key, Secret: e.Secret}
		if !e.Secret {
			value := e.Value
			entry.Value = &value
		}
		out.Config = append(out.Config, entry)
	}
	sort.Slice(out.Config, func(i, j int) bool { return out.Config[i].Key < out.Config[j].Key })
	if wrote {
		out.AppliesOnNextDeploy, out.Note = true, noteNextDeploy
	}
	return nil, out, nil
}

// resolveOwned is the ownership check all three tools start with. An app that
// does not exist and one belonging to another account are the same refusal, so
// no tool tells a caller whether someone else's app exists (AC-13).
func (s *Server) resolveOwned(ctx context.Context, account auth.Account, name, action string) (App, error) {
	if name == "" {
		return App{}, s.denyConfig(ctx, account.ID, "", action, domain.ReasonAppUnknown,
			errors.New("name is required"))
	}
	app, err := s.apps.ByName(ctx, account.ID, name)
	if errors.Is(err, ErrNoApp) {
		return App{}, s.denyConfig(ctx, account.ID, "", action, domain.ReasonAppUnknown, err)
	}
	if err != nil {
		return App{}, toolError(action, domain.ReasonInternal, fmt.Errorf("resolving app %q: %w", name, err))
	}
	return app, nil
}

// recordConfigChange writes the audit row one changed key leaves behind. The
// table carries one target pair and this change has two things worth naming, so
// the app id and the key travel joined. No row ever carries a value (AC-12).
func (s *Server) recordConfigChange(ctx context.Context, accountID, appID, key, action string) {
	auth.Record(ctx, s.auditor, auth.Audit{
		AccountID:  accountID,
		Action:     action,
		TargetType: auth.TargetAppConfig,
		TargetID:   appID + "/" + key,
		Allowed:    true,
	})
}

// denyConfig records the refusal and gives back the one line the caller sees.
func (s *Server) denyConfig(ctx context.Context, accountID, appID, action string, reason domain.Reason, cause error) error {
	entry := auth.Audit{AccountID: accountID, Action: action, Reason: string(reason)}
	if appID != "" {
		entry.TargetType, entry.TargetID = "app", appID
	}
	auth.Record(ctx, s.auditor, entry)
	return toolError(action, reason, cause)
}

// sortedKeys puts a caller's map in a fixed order, so a refusal names the same
// key every time and the audit rows land in a predictable sequence.
func sortedKeys(config map[string]configValue) []string {
	keys := make([]string, 0, len(config))
	for k := range config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
