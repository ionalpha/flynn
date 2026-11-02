//go:build linux || darwin

package sandbox

import (
	"os"
	"os/exec"
	"sort"
	"strings"
)

// grantScratch is the Linux and macOS counterpart of the Windows environment builder: for a
// command confined to a read-only host it redirects c's temp directory and toolchain caches
// into the scratch directory and returns the directories the launch must make writable, the
// scratch directory first. Without it the child inherits the host's TMPDIR and a cache path
// under the home directory, and the first `go test` or `npm install` fails on a read-only
// file system. It must run after c.Env holds the sandbox's environment and before the
// platform appends its own control variables.
func (l *Local) grantScratch(c *exec.Cmd) ([]string, error) {
	writable, err := l.writableTargets()
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(c.Env))
	for _, kv := range c.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			m[k] = v
		}
	}
	withScratchEnv(m, writable[0], os.Getenv)
	env := make([]string, 0, len(m))
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	c.Env = env
	return writable, nil
}
