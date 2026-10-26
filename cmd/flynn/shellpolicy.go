package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
)

// shellPostureEvent is the event type on a run's own stream that states what the
// commands the run launched were kept away from. It is written for every run, not only for
// one that opted out, so a record that lacks it came from a build that did not state its
// posture and a record that carries it can be read for the answer.
const shellPostureEvent = "sandbox.posture"

// shellPolicy is what a command the agent launches is kept away from, beyond the kernel
// confinement every command gets. The zero value is the strictest policy: no network, and
// the Flynn data directory plus the host's credential locations hidden. A caller relaxes it
// by setting a field, so an assembly that forgets to configure one is closed rather than
// open.
//
// The commands are model-authored, and the model reads what they print. Anything the
// confined shell can read it can be asked to echo, and anything it can reach on the network
// it can send out, so a login token or a signing key readable from the shell is exposed to
// whatever text the model has read.
type shellPolicy struct {
	// limits caps the memory and process count of each command.
	limits sandbox.ResourceLimits
	// dataDir is the Flynn data directory: the run database, the instance signing key and
	// the stored provider credentials.
	dataDir string
	// allowNetwork gives the commands the host's network. Without it they run in a network
	// namespace with no interfaces, which also takes away the loopback services of the host
	// (the control-plane API among them).
	allowNetwork bool
	// allowCredentialReads leaves the data directory and the credential locations readable.
	// It is the only way to run a shell on a platform that cannot hide a path (anything but
	// Linux today), where the hidden set makes every launch refuse.
	allowCredentialReads bool
}

// shellFlags are the command-line flags that set a [shellPolicy]. They are registered
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
func (f shellFlags) policy(dataDir string) shellPolicy {
	return shellPolicy{
		limits:               sandbox.ResourceLimits{MemoryMiB: *f.maxMemory, MaxProcesses: *f.maxProcs},
		dataDir:              dataDir,
		allowNetwork:         *f.allowNet,
		allowCredentialReads: *f.allowCreds,
	}
}

// options returns the sandbox options the policy stands for. Both restrictions apply on top
// of [sandbox.WithDefaultConfinement], so a platform that cannot enforce one refuses the
// launch instead of running the command without it.
func (p shellPolicy) options() []sandbox.LocalOption {
	opts := []sandbox.LocalOption{
		sandbox.WithDefaultConfinement(),
		sandbox.WithResourceLimits(p.limits),
	}
	if !p.allowNetwork {
		opts = append(opts, sandbox.WithNetworkDenied())
	}
	if !p.allowCredentialReads {
		opts = append(opts, sandbox.WithUnreadablePaths(p.hiddenPaths()...))
	}
	return opts
}

// hiddenPaths lists what the commands cannot read: the data directory, the runner's
// runtime directory, and the credential locations of the user the agent runs as.
func (p shellPolicy) hiddenPaths() []string {
	home, _ := os.UserHomeDir()
	paths := append([]string{p.dataDir}, credentialPaths(home, os.Getenv, os.Getuid())...)
	return dedupe(paths)
}

// credentialHomePaths are the places under a home directory where a tool keeps a login
// token, a private key or a password store. Each is a prefix: a directory hides everything
// beneath it.
var credentialHomePaths = []string{
	".ssh",
	".gnupg",
	".aws",
	".azure",
	".config/gcloud",
	".config/gh",
	".config/op",
	".config/git/credentials",
	".docker",
	".kube",
	".netrc",
	".npmrc",
	".pypirc",
	".git-credentials",
	".terraform.d",
	".cargo/credentials.toml",
	".local/share/keyrings",
	".password-store",
	".codex",
	".claude",
	".claude.json",
}

// credentialEnvPaths are the environment variables that relocate a credential store or name
// a socket to one. A value is a path (or, for KUBECONFIG, a list of them).
var credentialEnvPaths = []string{
	"SSH_AUTH_SOCK",
	"GNUPGHOME",
	"AWS_SHARED_CREDENTIALS_FILE",
	"AWS_CONFIG_FILE",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"CLOUDSDK_CONFIG",
	"DOCKER_CONFIG",
	"KUBECONFIG",
	"CODEX_HOME",
	"CLAUDE_CONFIG_DIR",
}

// runtimeSocketNames are the entries in a user's runtime directory that are keychain,
// agent and bus sockets: a command that can connect to one can ask the desktop's secret
// service or the user's agent for a secret without reading any file.
var runtimeSocketNames = []string{"bus", "keyring", "gnupg", "gcr", "ssh-agent.socket"}

// runnerRuntimeDir is where the runner's unit puts its sockets (RuntimeDirectory= in
// deploy/systemd/flynn-runner.service).
const runnerRuntimeDir = "/run/flynn"

// credentialPaths lists the credential locations for a user with the given home directory,
// environment and uid. It takes the environment as a function so the set can be computed
// for a user other than the process's, and so a test needs no real home.
func credentialPaths(home string, getenv func(string) string, uid int) []string {
	var paths []string
	if home != "" {
		for _, rel := range credentialHomePaths {
			paths = append(paths, filepath.Join(home, filepath.FromSlash(rel)))
		}
	}
	for _, name := range credentialEnvPaths {
		paths = append(paths, filepath.SplitList(getenv(name))...)
	}
	paths = append(paths, runnerRuntimeDir, getenv("RUNTIME_DIRECTORY"))
	runtime := getenv("XDG_RUNTIME_DIR")
	if runtime == "" && uid >= 0 {
		runtime = filepath.Join("/run/user", strconv.Itoa(uid))
	}
	if runtime != "" {
		for _, name := range runtimeSocketNames {
			paths = append(paths, filepath.Join(runtime, name))
		}
	}
	return paths
}

// dedupe drops empty and repeated entries, keeping the first of each in order.
func dedupe(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := paths[:0:0]
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// declareShellPosture records on stream what sb keeps its commands away from, read from the
// sandbox that will launch them. A relaxation the operator asked for is therefore in the
// sealed record next to the actions that ran under it. A record that cannot be written
// fails the assembly: a run must not start under a relaxed policy it cannot account for.
func declareShellPosture(ctx context.Context, log spine.Log, stream string, sb *sandbox.Local) error {
	network := "open"
	if sb.NetworkDenied() {
		network = "denied"
	}
	hidden := sb.UnreadablePaths()
	credentials := "hidden"
	if len(hidden) == 0 {
		credentials = "readable"
	}
	payload := map[string]any{
		"tier":        sb.ConfinementTier(),
		"network":     network,
		"credentials": credentials,
	}
	if len(hidden) > 0 {
		payload["hidden"] = hidden
	}
	_, err := log.Append(ctx, spine.AppendInput{
		Stream:  stream,
		Type:    shellPostureEvent,
		Actor:   spine.ActorSystem,
		Payload: payload,
	})
	return err
}
