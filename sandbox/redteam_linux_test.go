//go:build linux

package sandbox

import (
	"context"
	"testing"
)

// redteamEscapes are the Linux adapter's escape probes: a filesystem-write attempt, an
// outbound connect, and a forbidden syscall. The connect verdict is shared and want-aware;
// the syscall probe self-distinguishes by first confirming the syscall is available
// unconfined, so a host that forbids it regardless skips rather than misjudges.
func redteamEscapes() []redteamEscape {
	return []redteamEscape{
		fsWriteEscape(),
		egressEscape("timeout 5 bash -c 'exec 3<>/dev/tcp/8.8.8.8/53' 2>&1"),
		{
			name: "forbidden-syscall-unshare",
			axis: axisSyscall,
			run: func(t *testing.T, sb *Local, _, _ string, _ bool) (bool, string) {
				const probe = "unshare --user --map-root-user true"
				open, err := NewLocal(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				res, err := open.Exec(context.Background(), Command{Line: probe})
				if err != nil || res.ExitCode != 0 {
					return false, "unshare is unavailable or forbidden even unconfined; cannot judge syscall containment"
				}
				res, err = sb.Exec(context.Background(), Command{Line: probe})
				if err != nil {
					if namespaceUnavailable(err.Error()) {
						return false, "unprivileged user namespaces unavailable on this host"
					}
					return false, "syscall probe exec failed: " + err.Error()
				}
				return res.ExitCode != 0, "" // contained iff the syscall was denied
			},
		},
	}
}
