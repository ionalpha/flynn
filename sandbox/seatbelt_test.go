package sandbox

import (
	"strings"
	"testing"
)

// TestSeatbeltProfileNetworkDeny proves the network option emits the socket denial
// and the no-network option does not, so a command is cut off from the network only
// when the caller asked for it.
func TestSeatbeltProfileNetworkDeny(t *testing.T) {
	with := seatbeltProfile("/work", true, false, false)
	if !strings.Contains(with, "(deny network*)") {
		t.Fatalf("network-denied profile must deny the network, got:\n%s", with)
	}
	without := seatbeltProfile("/work", false, true, false)
	if strings.Contains(without, "(deny network*)") {
		t.Fatalf("a profile without network denial must not deny the network, got:\n%s", without)
	}
}

// TestSeatbeltProfileReadOnlyGrantsWorkingDir proves the read-only option denies all
// writes first and then re-grants the working directory, and that the denial comes
// before the grant so the last-match-wins ordering leaves the working tree writable.
func TestSeatbeltProfileReadOnlyGrantsWorkingDir(t *testing.T) {
	p := seatbeltProfile("/work/proj", false, true, false)
	denyAt := strings.Index(p, "(deny file-write*)")
	grantAt := strings.Index(p, `(subpath "/work/proj")`)
	if denyAt < 0 {
		t.Fatalf("read-only profile must deny writes, got:\n%s", p)
	}
	if grantAt < 0 {
		t.Fatalf("read-only profile must re-grant the working directory, got:\n%s", p)
	}
	if denyAt > grantAt {
		t.Fatalf("the blanket write denial must come before the working-directory grant (last match wins), got:\n%s", p)
	}
	for _, dev := range seatbeltWritableDevices {
		if !strings.Contains(p, `(literal "`+dev+`")`) {
			t.Fatalf("read-only profile must keep device %q writable, got:\n%s", dev, p)
		}
	}
	// The host temp area must not be writable: only the working tree and the listed
	// devices are, so nothing on the real filesystem outside the working tree is open.
	if strings.Contains(p, "/private/tmp") || strings.Contains(p, "/private/var/folders") {
		t.Fatalf("read-only profile must not grant the host temp area, got:\n%s", p)
	}
}

// TestSeatbeltProfileNoWriteDenialWithoutReadOnly proves writes are only denied under
// the read-only option, so a command not asked to run read-only keeps writing.
func TestSeatbeltProfileNoWriteDenialWithoutReadOnly(t *testing.T) {
	p := seatbeltProfile("/work", true, false, true)
	if strings.Contains(p, "(deny file-write*)") {
		t.Fatalf("a profile without the read-only option must not deny writes, got:\n%s", p)
	}
}

// TestSeatbeltProfileSyscallHardening proves the syscall option refuses the privileged
// operations a confined command never needs, and that the option is required for them.
func TestSeatbeltProfileSyscallHardening(t *testing.T) {
	p := seatbeltProfile("/work", false, false, true)
	for _, op := range []string{"file-write-setugid", "system-acct", "system-reboot", "system-set-time"} {
		if !strings.Contains(p, "(deny "+op+")") {
			t.Fatalf("hardened profile must deny %q, got:\n%s", op, p)
		}
	}
	off := seatbeltProfile("/work", true, true, false)
	if strings.Contains(off, "file-write-setugid") {
		t.Fatalf("a profile without the syscall option must not add the privileged-op denials, got:\n%s", off)
	}
}

// TestSeatbeltProfileEscapesPath proves a working directory containing a double-quote
// or backslash cannot break out of the SBPL string literal and inject profile rules:
// the dangerous characters are escaped, not passed through raw.
func TestSeatbeltProfileEscapesPath(t *testing.T) {
	p := seatbeltProfile(`/work/a"b\c`, false, true, false)
	if strings.Contains(p, `"/work/a"b\c"`) {
		t.Fatalf("a raw quote or backslash must not survive into the profile, got:\n%s", p)
	}
	if !strings.Contains(p, `(subpath "/work/a\"b\\c")`) {
		t.Fatalf("the path must be escaped into a single safe literal, got:\n%s", p)
	}
}

// TestSeatbeltProfileWellFormed proves the profile always opens with the version and
// allow-default header the launcher requires, regardless of which options are set.
func TestSeatbeltProfileWellFormed(t *testing.T) {
	p := seatbeltProfile("/work", true, true, true)
	if !strings.HasPrefix(p, "(version 1)\n(allow default)\n") {
		t.Fatalf("profile must open with the version and allow-default header, got:\n%s", p)
	}
}

// TestSBPLStringEscaping checks the literal escaper directly on the two characters
// that matter: the backslash and the double-quote.
func TestSBPLStringEscaping(t *testing.T) {
	cases := map[string]string{
		`/plain/path`: `"/plain/path"`,
		`/a"b`:        `"/a\"b"`,
		`/a\b`:        `"/a\\b"`,
		`/a\"b`:       `"/a\\\"b"`,
	}
	for in, want := range cases {
		if got := sbplString(in); got != want {
			t.Fatalf("sbplString(%q) = %q, want %q", in, got, want)
		}
	}
}
