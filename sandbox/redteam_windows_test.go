//go:build windows

package sandbox

// redteamEscapes are the Windows adapter's escape probes: a filesystem-write attempt and
// an outbound connect. Syscall filtering is a Linux concept; the Windows AppContainer
// confines the filesystem and network instead, so the syscall axis is not probed here.
func redteamEscapes() []redteamEscape {
	return []redteamEscape{
		fsWriteEscape(),
		egressEscape(`curl --max-time 6 -s -o NUL http://1.1.1.1`),
	}
}
