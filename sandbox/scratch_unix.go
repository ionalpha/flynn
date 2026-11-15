//go:build linux || darwin

package sandbox

import (
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

// grantScratch points temp and toolchain caches at scratch for a read-only host,
// and returns the directories to make writable, scratch first. Without it the
// first `go test` or `npm install` fails. It runs after c.Env is set and before the
// platform adds its control variables.
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
	withScratchEnv(m, writable[0], os.Getenv, runtime.GOOS)
	env := make([]string, 0, len(m))
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	c.Env = env
	return writable, nil
}
