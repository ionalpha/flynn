package main

import (
	"flag"

	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/sandbox"
)

// shellFlags are the command-line flags that set a [governed.ShellPolicy]. They are registered
// together and read through [shellFlags.policy], so the flags and the policy they build are
// one place and a flag added to one cannot be left out of the other.
type shellFlags struct {
	maxMemory, maxProcs  *int
	allowNet, allowCreds *bool
}

// registerShellFlags declares the shell policy flags on fs.
func registerShellFlags(fs *flag.FlagSet) shellFlags {
	return shellFlags{
		maxMemory:  fs.Int("max-memory", 0, "cap the memory (MiB) a command the agent runs may commit; 0 (default) is unlimited. Bounds a memory bomb; enforced where the platform supports it (a Windows job object today)."),
		maxProcs:   fs.Int("max-processes", 0, "cap how many processes a command the agent runs may spawn; 0 (default) uses the platform's generous fork-bomb backstop."),
		allowNet:   fs.Bool("allow-network", false, "give the commands the agent runs the host's network. By default they run with none, so a command cannot send what it reads anywhere; this is needed for git over the network, package installs and anything else a command fetches. The choice is recorded on the run's stream."),
		allowCreds: fs.Bool("allow-credential-reads", false, "leave the Flynn data directory and the host's credential locations (~/.ssh, cloud and CLI login tokens, keychain sockets) readable to the commands the agent runs. By default they are hidden. Needed on a platform that cannot hide a path (anything but Linux today), where the shell otherwise refuses to run. The choice is recorded on the run's stream."),
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
	}
}
