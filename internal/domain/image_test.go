package domain_test

import (
	"testing"

	"github.com/toyinogun/deployer/internal/domain"
)

// covers AC-10: an image that names no user is refused just as firmly as one
// that names root, because an empty USER runs as root.
func TestRunsAsRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		user string
		want bool
	}{
		{"", true},
		{"   ", true},
		{"0", true},
		{"root", true},
		{"0:0", true},
		{"root:root", true},
		{" 0 ", true},
		{"1000", false},
		{"cnb", false},
		{"1000:1000", false},
		{"nonroot", false},
	}
	for _, tc := range cases {
		t.Run(tc.user, func(t *testing.T) {
			t.Parallel()
			if got := domain.RunsAsRoot(tc.user); got != tc.want {
				t.Errorf("RunsAsRoot(%q) = %v, want %v", tc.user, got, tc.want)
			}
		})
	}
}
