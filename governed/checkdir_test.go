package governed

import (
	"os"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

// TestCheckDirIsTheDirectoryTheRunsSandboxIsBuiltOn: the worker refuses a check whose
// command a file in this directory stands in for, so it has to be the directory the check
// runs in. A host-supplied sandbox runs somewhere governed cannot see, and naming the
// process's directory for it would refuse checks over files the check never reads.
func TestCheckDirIsTheDirectoryTheRunsSandboxIsBuiltOn(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sb, err := sandbox.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	for _, tc := range []struct {
		name string
		c    config
		want string
	}{
		{"an explicit working directory", config{workdir: dir}, dir},
		{"the process's directory by default", config{}, cwd},
		{"a host sandbox", config{sandbox: sb}, ""},
		{"a host sandbox beside a working directory", config{sandbox: sb, workdir: dir}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.checkDir(); got != tc.want {
				t.Fatalf("checkDir() = %q, want %q", got, tc.want)
			}
		})
	}
}
