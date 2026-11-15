package mission

import "runtime"

// HostNote names the OS and the shell that reads every command and ledger check. An
// untold model writes POSIX checks, which cmd.exe cannot run, so on Windows the item could
// never be proven. The planner writes checks and the builder writes commands, so both
// prompts carry it.
func HostNote() string { return hostNote(runtime.GOOS) }

// hostNote takes GOOS so each platform's wording is testable on any host.
func hostNote(goos string) string {
	switch goos {
	case "windows":
		return "Host: Windows. Every command you run, and every check, is read by cmd.exe (cmd /c), not by a POSIX shell. " +
			"Write cmd.exe syntax. test, grep, sed, awk, cat, ls, head, touch and $(...) substitution do not exist, and && and || work but single quotes do not quote. " +
			"For a check, prefer a program that exits 0 or non-zero by itself, such as `python hello.py` or `go test ./...`, and a check that names a program the host lacks can never pass."
	case "darwin":
		return "Host: macOS. Every command you run, and every check, is read by sh -c, with the BSD versions of the usual tools (sed and date differ from GNU)."
	default:
		return "Host: " + goos + ". Every command you run, and every check, is read by sh -c, so POSIX shell syntax applies."
	}
}
