package main

import (
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

func TestGrantStatusText(t *testing.T) {
	tests := []struct {
		name string
		p    sandbox.GrantProgress
		want string
	}{
		{"start", sandbox.GrantProgress{Dir: `C:\repo`}, `granting the sandbox write access to C:\repo...`},
		{"counting", sandbox.GrantProgress{Dir: `C:\repo`, Files: 1200}, `granting the sandbox write access to C:\repo: 1200 files...`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := grantStatusText(tc.p); got != tc.want {
				t.Fatalf("grantStatusText = %q, want %q", got, tc.want)
			}
		})
	}
}
