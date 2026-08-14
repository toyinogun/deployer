package deploy

// The whole TCP port space, which the blocked list is complemented over.
const (
	firstPort = int32(1)
	lastPort  = int32(65535)
)

// PortRange is one inclusive span of TCP ports an app may reach on a public
// address. Every range carries both ends, so a span one port wide reads as
// EndPort equal to Port rather than as an entry with EndPort unset: one shape for
// every composed entry (spec 0017, AC-2).
type PortRange struct {
	Port    int32
	EndPort int32
}

// AllowedPortRanges is the complement of blocked over 1..65535, in order.
//
// This is the whole of the port bound. A NetworkPolicy can only ever permit a
// port, never refuse one, so a blocked port is a port that appears in no range
// here, and nothing else may add a port to the policy or the configured list
// stops being the single description of what an app may reach (spec 0017, Key
// invariants).
//
// blocked is expected sorted and deduplicated, which internal/config does at
// startup, but a duplicate or an out of order entry only ever narrows what comes
// back: no range is emitted unless it is at least one port wide, so a repeat
// never produces an inverted or zero width entry.
func AllowedPortRanges(blocked []int32) []PortRange {
	var ranges []PortRange

	// next is the lowest port not yet accounted for. It walks past each blocked
	// port, and a gap becomes a range only when there is one.
	next := firstPort
	for _, port := range blocked {
		if port < next || port > lastPort {
			continue
		}
		if port > next {
			ranges = append(ranges, PortRange{Port: next, EndPort: port - 1})
		}
		next = port + 1
	}
	if next <= lastPort {
		ranges = append(ranges, PortRange{Port: next, EndPort: lastPort})
	}
	return ranges
}
