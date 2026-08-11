// Package domain holds the platform's business rules: the deployment state
// machine and slug derivation. It imports nothing from the store, HTTP, or
// Kubernetes, so the rules can be read and tested on their own.
package domain

// State is where a deployment currently is. The database CHECK constraint
// polices the set of values; this package polices the moves between them.
type State string

// The seven deployment states (spec 0002).
const (
	StateQueued    State = "queued"
	StateBuilding  State = "building"
	StatePushing   State = "pushing"
	StateDeploying State = "deploying"
	StateHealthy   State = "healthy"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// allowed is the whole transition table. A rollback takes the queued to
// deploying shortcut because it re promotes a digest that already exists, so it
// never needs building or pushing. Terminal states have no entry at all.
var allowed = map[State][]State{
	StateQueued:    {StateBuilding, StateDeploying, StateFailed, StateCancelled},
	StateBuilding:  {StatePushing, StateFailed, StateCancelled},
	StatePushing:   {StateDeploying, StateFailed, StateCancelled},
	StateDeploying: {StateHealthy, StateFailed, StateCancelled},
}

// Valid reports whether s is one of the seven states.
func (s State) Valid() bool {
	if s == StateHealthy || s == StateFailed || s == StateCancelled {
		return true
	}
	_, ok := allowed[s]
	return ok
}

// Terminal reports whether no transition leaves s.
func (s State) Terminal() bool {
	return s == StateHealthy || s == StateFailed || s == StateCancelled
}

// CanTransition reports whether a deployment may move from one state to another.
// A move out of a terminal state, or to or from an unknown state, is never legal.
func CanTransition(from, to State) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	for _, s := range allowed[from] {
		if s == to {
			return true
		}
	}
	return false
}
