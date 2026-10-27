package main

import (
	"flag"
	"fmt"

	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/sandbox"
)

// shellFlags are the command-line flags that set a [governed.ShellPolicy]. They are registered
// together and read through [shellFlags.policy], so the flags and the policy they build are
// one place and a flag added to one cannot be left out of the other.
type shellFlags struct {
	maxMemory, maxProcs                 *int
	allowNet, allowCreds, requireHidden *bool
}

// registerShellFlags declares the shell policy flags on fs.
func registerShellFlags(fs *flag.FlagSet) shellFlags {
	return shellFlags{
		maxMemory:     fs.Int("max-memory", 0, "cap the memory (MiB) a command the agent runs may commit; 0 (default) is unlimited. Bounds a memory bomb; enforced where the platform supports it (a Windows job object today)."),
		maxProcs:      fs.Int("max-processes", 0, "cap how many processes a command the agent runs may spawn; 0 (default) uses the platform's generous fork-bomb backstop."),
		allowNet:      fs.Bool("allow-network", false, "give the commands the agent runs the host's network. By default they run with none, so a command cannot send what it reads anywhere; this is needed for git over the network, package installs and anything else a command fetches. The choice is recorded on the run's stream."),
		allowCreds:    fs.Bool("allow-credential-reads", false, "leave the Flynn data directory and the host's credential locations (~/.ssh, cloud and CLI login tokens, keychain sockets) readable to the commands the agent runs. By default they are hidden on Linux and macOS; Windows cannot hide a path from a command, so there they stay readable unless --require-credential-hiding is passed. The choice is recorded on the run's stream."),
		requireHidden: fs.Bool("require-credential-hiding", false, "refuse to run a command where the platform cannot hide the Flynn data directory and the credential locations from it (Windows today), rather than running it with them readable. Has no effect on Linux and macOS, which always hide them unless --allow-credential-reads is passed."),
	}
}

// policy is the shell policy the parsed flags ask for, for a run whose state lives in
// dataDir.
func (f shellFlags) policy(dataDir string) governed.ShellPolicy {
	return governed.ShellPolicy{
		Limits:               sandbox.ResourceLimits{MemoryMiB: *f.maxMemory, MaxProcesses: *f.maxProcs},
		DataDir:              dataDir,
		AllowNetwork:         *f.allowNet,
		AllowCredentialReads: *f.allowCreds,
		// The binary runs on every OS it ships for: where a path cannot be hidden it runs
		// the shell with the credentials readable and says so, rather than refusing every
		// command, unless the operator asked for the refusal.
		AllowCredentialReadsWhereUnhidable: !*f.requireHidden,
	}
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
}
