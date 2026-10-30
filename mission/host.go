package mission

import "runtime"

// HostNote tells a model which operating system the commands it writes will run on and
// which shell reads them. The bash tool and every ledger check are handed to one shell
// (cmd.exe on Windows, sh elsewhere), and a model that is not told reaches for the shell
// it was trained on: on Windows that is a POSIX check such as `test "$(python hello.py)" =
// "hello"`, which cmd.exe cannot run, so the item can never be proven however
// well the work is done.
//
// The planner and the build prompt both carry it, because the planner is the one that
// writes the check and the builder is the one that writes the commands.
func HostNote() string { return hostNote(runtime.GOOS) }

// hostNote is HostNote for a named GOOS, so each platform's wording is testable on any
// host.
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
