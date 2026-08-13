package domain

// The bounds one app's configuration lives inside. Constants rather than
// DEPLOYER_* variables for the same reason the log bounds are: they are product
// decisions about what an app should carry, not knobs for whoever runs the
// platform (spec 0010, Configuration required).
const (
	// MaxConfigKeys is how many keys one app may hold.
	MaxConfigKeys = 64
	// MaxConfigValueBytes is the largest single value.
	MaxConfigValueBytes = 4 << 10
	// MaxConfigTotalBytes is the largest an app's whole configuration may be,
	// counting each key and its value.
	MaxConfigTotalBytes = 32 << 10
)

// The keys the platform composes itself and a caller may therefore never store.
// What the container sees for these never depends on what a caller sent
// (spec 0010, AC-5).
const (
	ReservedKeyPort   = "PORT"
	ReservedKeyAppURL = "APP_URL"
)

// ConfigWrite is one key a caller asked to set. Secret is a pointer because its
// absence is a refusal rather than a default: a key that reached the platform
// without it would otherwise quietly turn a secret into a plain value (AC-16).
type ConfigWrite struct {
	Key    string
	Value  string
	Secret *bool
}

// IsSecret reports the flag, treating an absent one as not secret. Only call it
// after ValidateConfig has accepted the write, which is what makes the absent
// case unreachable.
func (w ConfigWrite) IsSecret() bool { return w.Secret != nil && *w.Secret }

// ValidateConfig checks a whole set_config or deploy_app call before anything is
// written, and returns the one reason code it is refused with, or the empty
// Reason when it may proceed.
//
// existing is the app's current keys and values, needed because the key count
// and the total size are bounds on the merged result rather than on the call.
// The whole call is judged before any of it is written, so a call carrying five
// keys where one is bad writes none of the five (AC-1, AC-6).
func ValidateConfig(writes []ConfigWrite, existing map[string]string) Reason {
	if len(writes) == 0 {
		return ReasonConfigKeyInvalid
	}
	seen := make(map[string]struct{}, len(writes))
	for _, w := range writes {
		if !ValidConfigKey(w.Key) {
			return ReasonConfigKeyInvalid
		}
		if w.Key == ReservedKeyPort || w.Key == ReservedKeyAppURL {
			return ReasonConfigKeyReserved
		}
		if w.Secret == nil {
			return ReasonConfigFlagMissing
		}
		// One call naming the same key twice has two answers for it and no way to
		// pick, which is the caller's mistake rather than a merge rule.
		if _, dupe := seen[w.Key]; dupe {
			return ReasonConfigKeyInvalid
		}
		seen[w.Key] = struct{}{}
		if len(w.Value) > MaxConfigValueBytes {
			return ReasonConfigTooLarge
		}
	}

	merged := make(map[string]string, len(existing)+len(writes))
	for k, v := range existing {
		merged[k] = v
	}
	for _, w := range writes {
		merged[w.Key] = w.Value
	}
	if len(merged) > MaxConfigKeys {
		return ReasonConfigTooManyKeys
	}
	total := 0
	for k, v := range merged {
		total += len(k) + len(v)
	}
	if total > MaxConfigTotalBytes {
		return ReasonConfigTooLarge
	}
	return ""
}

// ValidateUnset checks an unset_config call's keys. Whether the keys are set is
// the store's answer, not this one: this only refuses a key that could never
// have been stored (AC-3, AC-4).
func ValidateUnset(keys []string) Reason {
	if len(keys) == 0 {
		return ReasonConfigKeyInvalid
	}
	for _, k := range keys {
		if !ValidConfigKey(k) {
			return ReasonConfigKeyInvalid
		}
	}
	return ""
}
