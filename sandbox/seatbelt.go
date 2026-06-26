package sandbox

import "strings"

// seatbeltProfile builds the sandbox profile (SBPL) that the macOS adapter applies
// to a command, from the same confinement options the Linux adapter reads. The
// profile is the macOS counterpart of the network namespace, read-only mount view,
// and syscall filter the Linux adapter installs: it denies the network, makes the
// host read-only except the working directory and a scratch area, and refuses the
// privileged file operations a command working in its own tree never needs.
//
// The profile starts from allow-default and removes capabilities, rather than
// starting from deny-default and re-granting them. A deny-default profile on macOS
// has to enumerate every operation an ordinary shell and its tools legitimately make
// (process spawn, dynamic linking, sysctl reads, mach lookups, and more), and the
// exact set shifts between OS releases; missing one turns a benign command into a
// confinement failure. Removing the specific capabilities that matter for isolation -
// the network, writes outside the working tree, and privilege escalation - keeps the
// load-bearing denials exact while leaving ordinary work running.
//
// Profile rules are last-match-wins, so a later rule overrides an earlier one: the
// blanket write denial comes first and the working-directory and scratch re-grants
// follow it.
func seatbeltProfile(root string, denyNetwork, readonlyFS, hardenSyscalls bool) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")

	if denyNetwork {
		// No outbound or inbound socket: the command cannot exfiltrate, phone home, or
		// accept a connection, the macOS counterpart of the empty network namespace.
		b.WriteString("(deny network*)\n")
	}

	if readonlyFS {
		// Every write is denied first, then re-granted only for the working tree and a
		// small set of device files, so the command cannot modify any host file outside
		// the directory it was given. Reads stay allowed (allow-default), matching the
		// Linux read-only host view where the whole filesystem is readable but only the
		// working tree is writable. The host temp directories are deliberately not
		// granted: a command that needs scratch space writes it inside the working tree,
		// and the rest of the host, including the shared temp area, stays read-only.
		b.WriteString("(deny file-write*)\n")
		b.WriteString("(allow file-write*\n")
		b.WriteString("    (subpath " + sbplString(root) + ")\n")
		for _, dev := range seatbeltWritableDevices {
			b.WriteString("    (literal " + sbplString(dev) + ")\n")
		}
		b.WriteString(")\n")
	}

	if hardenSyscalls {
		// The syscall-filter counterpart: refuse the privileged operations a command
		// confined to its own directory has no honest need for and that would let it
		// escalate privilege or tamper with the host. Ordinary file, process, and memory
		// work is untouched, so normal commands run unaffected.
		b.WriteString("(deny file-write-setugid)\n")
		b.WriteString("(deny system-acct)\n")
		b.WriteString("(deny system-reboot)\n")
		b.WriteString("(deny system-set-time)\n")
	}

	return b.String()
}

// seatbeltWritableDevices are the only host files outside the working tree a command
// may write under the read-only host view: the standard device endpoints a shell and
// its tools need to redirect output and read entropy. They are individual files, not
// directories, so the grant opens no writable area on the real filesystem. The
// working directory is granted separately as the one writable tree.
var seatbeltWritableDevices = []string{
	"/dev/null",
	"/dev/zero",
	"/dev/stdout",
	"/dev/stderr",
	"/dev/tty",
	"/dev/random",
	"/dev/urandom",
}

// sbplString renders a path as an SBPL double-quoted string literal, escaping the
// backslash and double-quote that would otherwise break the literal. A crafted path
// can therefore not close the string early and inject profile rules.
func sbplString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
