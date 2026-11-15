package sandbox

import (
	"net"
	"strings"
)

// seatbeltProfile builds the macOS SBPL equivalent of the Linux namespaces,
// read-only mounts and syscall filter.
//
// It starts from allow-default because a deny-default profile must list every
// operation a shell makes (spawn, linking, sysctl, mach lookups), and that set
// changes between releases. Rules are last-match-wins, so the write denial comes
// before the re-grants. A non-empty proxyAddr permits only the loopback proxy;
// writable holds the WithWritableDir directories.
func seatbeltProfile(root string, denyNetwork, readonlyFS, hardenSyscalls bool, proxyAddr string, writable ...string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")

	switch {
	case proxyAddr != "":
		// The kernel refuses every other outbound connection, so the child cannot
		// bypass the proxy.
		b.WriteString("(deny network*)\n")
		if _, port, err := net.SplitHostPort(proxyAddr); err == nil && port != "" {
			b.WriteString("(allow network-outbound (remote ip " + sbplString("localhost:"+port) + "))\n")
		}
	case denyNetwork:
		// No socket either way, like an empty network namespace.
		b.WriteString("(deny network*)\n")
	}

	if readonlyFS {
		// Writes are re-granted only for the working tree and a few device files.
		// The host temp directories stay read-only, so scratch goes in the tree.
		b.WriteString("(deny file-write*)\n")
		b.WriteString("(allow file-write*\n")
		b.WriteString("    (subpath " + sbplString(root) + ")\n")
		for _, w := range writable {
			b.WriteString("    (subpath " + sbplString(w) + ")\n")
		}
		for _, dev := range seatbeltWritableDevices {
			b.WriteString("    (literal " + sbplString(dev) + ")\n")
		}
		b.WriteString(")\n")
	}

	if hardenSyscalls {
		// The syscall filter's counterpart refuses privileged operations only.
		b.WriteString("(deny file-write-setugid)\n")
		b.WriteString("(deny system-acct)\n")
		b.WriteString("(deny system-reboot)\n")
		b.WriteString("(deny system-set-time)\n")
	}

	return b.String()
}

// seatbeltHidden rules go last, so no grant reaches a hidden path. The command
// sees a permission error where Linux shows an empty directory. A subpath rule
// also matches a file not yet created, such as a credential written mid-run.
func seatbeltHidden(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("(deny file-read* file-write*\n")
	for _, p := range paths {
		b.WriteString("    (subpath " + sbplString(p) + ")\n")
	}
	b.WriteString(")\n")
	return b.String()
}

// seatbeltWritableDevices are single files a shell needs for redirection and
// entropy, so they open no writable directory.
var seatbeltWritableDevices = []string{
	"/dev/null",
	"/dev/zero",
	"/dev/stdout",
	"/dev/stderr",
	"/dev/tty",
	"/dev/random",
	"/dev/urandom",
}

// sbplString escapes backslash and double quote, so a crafted path cannot inject
// profile rules.
func sbplString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
