//go:build darwin

package sandbox

// redteamEscapes are the macOS adapter's escape probes: a filesystem-write attempt and an
// outbound connect. The Seatbelt profile confines the filesystem and network rather than
// filtering syscalls, so the syscall axis is not probed here.
func redteamEscapes() []redteamEscape {
	return []redteamEscape{
		fsWriteEscape(),
		egressEscape("curl --max-time 5 -sS http://1.1.1.1 >/dev/null 2>&1"),
	}
}
