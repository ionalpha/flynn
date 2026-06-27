//go:build darwin

package sandbox

// redteamEscapes are the macOS adapter's escape probes: a filesystem-write attempt and an
// outbound connect. The Seatbelt profile confines the filesystem and network rather than
// filtering syscalls, so the syscall axis is not probed here.
func redteamEscapes() []redteamEscape {
	return []redteamEscape{
		fsWriteEscape(),
		// -f makes an HTTP error status (the proxy's 403 under a deny policy) a non-zero
		// exit, so reaching the proxy but being refused counts as contained, not as a
		// successful connection. A direct dial under egress confinement is blocked by the
		// kernel and also exits non-zero.
		egressEscape("curl -f --max-time 5 -sS http://1.1.1.1 >/dev/null 2>&1"),
	}
}
