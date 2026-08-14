package deploy_test

import (
	"testing"

	"github.com/toyinogun/deployer/internal/deploy"
)

// The complement is the only thing that decides what an app may reach, and a
// NetworkPolicy has no way to say "not this port", so every case below is a way
// the bound could silently stop being one (spec 0017, AC-2).
func TestAllowedPortRanges(t *testing.T) {
	for name, tc := range map[string]struct {
		blocked []int32
		want    []deploy.PortRange
	}{
		"nothing blocked is the whole space": {
			blocked: nil,
			want:    []deploy.PortRange{{Port: 1, EndPort: 65535}},
		},
		"the default list": {
			blocked: []int32{25, 3333, 4444, 5555, 7777, 9999, 14444},
			want: []deploy.PortRange{
				{Port: 1, EndPort: 24},
				{Port: 26, EndPort: 3332},
				{Port: 3334, EndPort: 4443},
				{Port: 4445, EndPort: 5554},
				{Port: 5556, EndPort: 7776},
				{Port: 7778, EndPort: 9998},
				{Port: 10000, EndPort: 14443},
				{Port: 14445, EndPort: 65535},
			},
		},
		// Nothing sits between them, so the gap must not become a range at all
		// rather than an inverted or zero width one.
		"adjacent blocked ports leave no range between": {
			blocked: []int32{80, 81, 82},
			want: []deploy.PortRange{
				{Port: 1, EndPort: 79},
				{Port: 83, EndPort: 65535},
			},
		},
		// Defended here as well as in config, because the function is the thing
		// that would emit an inverted range if a duplicate reached it.
		"duplicates are harmless": {
			blocked: []int32{80, 80, 80},
			want: []deploy.PortRange{
				{Port: 1, EndPort: 79},
				{Port: 81, EndPort: 65535},
			},
		},
		"blocking the first port emits no range starting at zero": {
			blocked: []int32{1},
			want:    []deploy.PortRange{{Port: 2, EndPort: 65535}},
		},
		"blocking the last port emits no range ending past it": {
			blocked: []int32{65535},
			want:    []deploy.PortRange{{Port: 1, EndPort: 65534}},
		},
		"both boundaries at once": {
			blocked: []int32{1, 65535},
			want:    []deploy.PortRange{{Port: 2, EndPort: 65534}},
		},
		// One shape for every entry: a single port is a range whose ends match,
		// never an entry with EndPort left unset.
		"a one port wide range carries EndPort equal to Port": {
			blocked: []int32{1, 3},
			want: []deploy.PortRange{
				{Port: 2, EndPort: 2},
				{Port: 4, EndPort: 65535},
			},
		},
		"everything blocked leaves nothing open": {
			blocked: allPorts(),
			want:    nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := deploy.AllowedPortRanges(tc.blocked)
			if len(got) != len(tc.want) {
				t.Fatalf("ranges = %v, want %v", got, tc.want)
			}
			for i, r := range tc.want {
				if got[i] != r {
					t.Errorf("range[%d] = %v, want %v", i, got[i], r)
				}
			}
			for i, r := range got {
				if r.EndPort < r.Port {
					t.Errorf("range[%d] = %v is inverted", i, r)
				}
				if r.Port < 1 || r.EndPort > 65535 {
					t.Errorf("range[%d] = %v leaves the port space", i, r)
				}
			}
		})
	}
}

// The composed ranges must cover every port that is not blocked and no port that
// is, which is the property the literal cases above only sample.
func TestAllowedPortRangesCoverExactlyTheComplement(t *testing.T) {
	blocked := []int32{1, 25, 26, 3333, 65534, 65535}
	open := map[int32]bool{}
	for _, r := range deploy.AllowedPortRanges(blocked) {
		for p := r.Port; p <= r.EndPort; p++ {
			if open[p] {
				t.Fatalf("port %d appears in more than one range", p)
			}
			open[p] = true
		}
	}
	isBlocked := map[int32]bool{}
	for _, p := range blocked {
		isBlocked[p] = true
	}
	for p := int32(1); p <= 65535; p++ {
		if open[p] == isBlocked[p] {
			t.Fatalf("port %d: open = %v, blocked = %v", p, open[p], isBlocked[p])
		}
	}
}

func allPorts() []int32 {
	all := make([]int32, 0, 65535)
	for p := int32(1); p <= 65535; p++ {
		all = append(all, p)
	}
	return all
}
