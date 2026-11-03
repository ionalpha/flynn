package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/internal/hardware"
	"github.com/ionalpha/flynn/sandbox"
)

// shellFlags are the command-line flags that set a [governed.ShellPolicy]. They are registered
// together and read through [shellFlags.policy], so the flags and the policy they build are
// one place and a flag added to one cannot be left out of the other.
type shellFlags struct {
	maxMemory, maxProcs                 *int
	allowNet, allowCreds, requireHidden *bool
	requireNetDenial                    *bool
}

// registerShellFlags declares the shell policy flags on fs.
func registerShellFlags(fs *flag.FlagSet) shellFlags {
	return shellFlags{
		maxMemory:        fs.Int("max-memory", 0, "cap the memory (MiB) a command the agent runs may commit; 0 (default) caps it at half this machine's physical memory. Bounds a memory bomb or a command that keeps starting copies of itself; enforced where the platform supports it (a Windows job object today)."),
		maxProcs:         fs.Int("max-processes", 0, "cap how many processes a command the agent runs may spawn; 0 (default) uses the platform's generous fork-bomb backstop."),
		allowNet:         fs.Bool("allow-network", false, "give the commands the agent runs the host's network. By default they run with none, so a command cannot send what it reads anywhere; this is needed for git over the network, package installs and anything else a command fetches. Windows cannot deny the network, so there they have it unless --require-network-denial is passed. The choice is recorded on the run's stream."),
		allowCreds:       fs.Bool("allow-credential-reads", false, "leave the Flynn data directory and the host's credential locations (~/.ssh, cloud and CLI login tokens, keychain sockets) readable to the commands the agent runs. By default they are hidden on Linux and macOS; Windows cannot hide a path from a command, so there they stay readable unless --require-credential-hiding is passed. The choice is recorded on the run's stream."),
		requireHidden:    fs.Bool("require-credential-hiding", false, "refuse to run a command where the platform cannot hide the Flynn data directory and the credential locations from it (Windows today), rather than running it with them readable. Has no effect on Linux and macOS, which always hide them unless --allow-credential-reads is passed."),
		requireNetDenial: fs.Bool("require-network-denial", false, "refuse to run a command where the platform cannot deny it the network (Windows today), rather than running it with the host's network. Has no effect on Linux and macOS, which always deny it unless --allow-network is passed."),
	}
}

// policy is the shell policy the parsed flags ask for, for a run whose state lives in
// dataDir.
func (f shellFlags) policy(dataDir string) governed.ShellPolicy {
	return governed.ShellPolicy{
		Limits:               commandLimits(*f.maxMemory, *f.maxProcs),
		DataDir:              dataDir,
		AllowNetwork:         *f.allowNet,
		AllowCredentialReads: *f.allowCreds,
		// The binary runs on every OS it ships for: where a path cannot be hidden it runs
		// the shell with the credentials readable and says so, rather than refusing every
		// command, unless the operator asked for the refusal.
		AllowCredentialReadsWhereUnhidable: !*f.requireHidden,
		AllowNetworkWhereUndeniable:        !*f.requireNetDenial,
	}
}

// hostRAMBytes reads the machine's physical memory. It is a variable so a test can give
// the default cap a known machine to be computed against.
var hostRAMBytes = hardware.RAMBytes

// commandLimits is the resource cap on every command the agent runs, from the
// --max-memory and --max-processes values. A memory value of 0 does not mean unlimited:
// it means half the machine's physical memory. The sandbox's process cap alone does not
// protect the host. A command that keeps starting copies of itself (a PATH shim that
// resolves back to itself, for one) can commit all of RAM with a few hundred heavy
// processes, far below the process backstop, and take the operator's machine down with
// the run. Half leaves the host, and this process, room to stop the run and report it.
//
// The default is set here, on the agent's shell policy, and not inside [sandbox.Local],
// because the sandbox also runs the local model server, which can legitimately need more
// than half the machine's memory. Where the RAM cannot be read the command runs uncapped,
// as before.
func commandLimits(memoryMiB, maxProcesses int) sandbox.ResourceLimits {
	if memoryMiB <= 0 {
		memoryMiB = int(hostRAMBytes(context.Background()) / 2 >> 20)
	}
	return sandbox.ResourceLimits{MemoryMiB: memoryMiB, MaxProcesses: maxProcesses}
}

// networkNotice is what an operator is told before a run whose commands have the host's
// network only because this platform cannot deny it, or "" for any other run. The posture
// event records it with the reason.
func networkNotice(p governed.ShellPolicy) string {
	if _, reason := p.NetworkOpen(); reason != governed.NetworkOpenByPlatform {
		return ""
	}
	return "notice: this platform cannot deny the network to the commands the agent runs, so they can reach it and send out anything they can read. The run's record says so."
}

// credentialNotice is what an operator is told before a run whose commands can read the
// credential locations only because this platform cannot hide them, or "" for any other
// run. A relaxation the operator asked for is not announced: they typed it. The posture
// event records both, with the reason.
func credentialNotice(p governed.ShellPolicy) string {
	if _, reason := p.CredentialsReadable(); reason != governed.CredentialsReadableByPlatform {
		return ""
	}
	return "notice: this platform cannot hide paths from the commands the agent runs, so they can read the Flynn data directory and the host's credential locations (~/.ssh, cloud and CLI login tokens). The run's record says so."
}

// noteShellPosture prints [credentialNotice] for the invocation's policy to stderr, for an
// entry point that is about to drive a shell.
func (inv invocation) noteShellPosture() {
	if n := credentialNotice(inv.shell); n != "" {
		_, _ = fmt.Fprintln(inv.stderr, n, "Pass --require-credential-hiding to refuse to run commands instead.")
	}
	if n := networkNotice(inv.shell); n != "" {
		_, _ = fmt.Fprintln(inv.stderr, n, "Pass --require-network-denial to refuse to run commands instead.")
	}
}
