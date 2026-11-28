package governed

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

// TestGrantWorkdirFollowsThePolicy makes the grant under the policy the commands
// run under: one that would refuse them grants nothing and says why.
func TestGrantWorkdirFollowsThePolicy(t *testing.T) {
	runs := ShellPolicy{AllowCredentialReadsWhereUnhidable: true, AllowNetworkWhereUndeniable: true}
	if err := runs.GrantWorkdir(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("a policy that runs commands: %v", err)
	}
	if sandbox.ReadDenySupported() {
		return
	}
	refuses := ShellPolicy{AllowNetworkWhereUndeniable: true}
	if err := refuses.GrantWorkdir(context.Background(), t.TempDir()); err == nil {
		t.Fatal("a policy that refuses every command granted the directory")
	}
}
