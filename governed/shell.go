package governed

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
)

// ShellPostureEventType is the event that states what a run's commands were kept away
// from. Every run that builds its own sandbox writes it, before the session opens, so a
// record without it came from a build that did not state its posture.
const ShellPostureEventType = "sandbox.posture"

// ShellPolicy is what a launched command is kept away from beyond kernel confinement. The
// zero value is the strictest: no network and credential locations hidden, so an assembly
// that forgets to configure it is closed. It applies only to a sandbox the run builds; a
// [WithSandbox] run is the host's to confine.
//
// The model writes the commands and reads their output, so any credential the shell can
// read or send over the network is exposed to whatever text the model has read.
//
//flynn:policy
type ShellPolicy struct {
	Limits sandbox.ResourceLimits
	// DataDir is the host's state directory (run database, signing key, provider
	// credentials), hidden along with the credential locations. Empty names none.
	DataDir string
	// AllowNetwork gives the commands the host's network. Without it they also lose the
	// host's loopback services, a control-plane API among them.
	AllowNetwork bool
	// AllowCredentialReads leaves DataDir and the credential locations readable everywhere.
	AllowCredentialReads bool
	// AllowCredentialReadsWhereUnhidable leaves them readable only where
	// [sandbox.ReadDenySupported] is false (Windows). Without it such a platform refuses
	// every launch; with it the posture event records the platform as the reason. The
	// binary sets it so a first run works on every OS.
	AllowCredentialReadsWhereUnhidable bool
	// AllowNetworkWhereUndeniable opens the network only where [sandbox.NetworkDenySupported]
	// is false (Windows). Without it such a platform refuses a no-network launch, so the
	// record never claims a denial that did not happen; with it the posture event records
	// the platform as the reason.
	AllowNetworkWhereUndeniable bool
}

// Posture reasons for an open network on a [ShellPostureEventType] event.
const (
	NetworkOpenByPolicy   = "policy"
	NetworkOpenByPlatform = "platform"
)

// NetworkOpen reports whether the commands get the host's network here, and the posture
// reason recorded. The sandbox options and the posture event both read it, so a warning
// to an operator says what the record will say.
func (p ShellPolicy) NetworkOpen() (open bool, reason string) {
	switch {
	case p.AllowNetwork:
		return true, NetworkOpenByPolicy
	case p.AllowNetworkWhereUndeniable && !sandbox.NetworkDenySupported():
		return true, NetworkOpenByPlatform
	}
	return false, ""
}

// Posture reasons for readable credentials on a [ShellPostureEventType] event.
const (
	CredentialsReadableByPolicy   = "policy"
	CredentialsReadableByPlatform = "platform"
)

// CredentialsReadable reports whether the commands can read the credential locations here,
// and the posture reason recorded, read by the same callers as [ShellPolicy.NetworkOpen].
func (p ShellPolicy) CredentialsReadable() (readable bool, reason string) {
	switch {
	case p.AllowCredentialReads:
		return true, CredentialsReadableByPolicy
	case p.AllowCredentialReadsWhereUnhidable && !sandbox.ReadDenySupported():
		return true, CredentialsReadableByPlatform
	}
	return false, ""
}

// WithShellPolicy sets the run's own sandbox policy; the default is the zero [ShellPolicy].
func WithShellPolicy(p ShellPolicy) Option {
	return func(c *config) { c.shell, c.shellSet = p, true }
}

// options stacks the restrictions on [sandbox.WithDefaultConfinement], so a platform that
// cannot enforce one refuses the launch instead of running without it.
func (p ShellPolicy) options() []sandbox.LocalOption {
	opts := []sandbox.LocalOption{
		sandbox.WithDefaultConfinement(),
		sandbox.WithResourceLimits(p.Limits),
	}
	if !p.AllowNetwork {
		opts = append(opts, sandbox.WithNetworkDenied())
		if p.AllowNetworkWhereUndeniable {
			opts = append(opts, sandbox.WithOpenNetworkWhereUndeniable())
		}
	}
	if readable, _ := p.CredentialsReadable(); !readable {
		opts = append(opts, sandbox.WithUnreadablePaths(p.hiddenPaths()...))
	}
	return opts
}

func (p ShellPolicy) hiddenPaths() []string {
	home, _ := os.UserHomeDir()
	paths := append([]string{p.DataDir}, credentialPaths(home, os.Getenv, os.Getuid())...)
	return dedupe(paths)
}

// credentialHomePaths are home-relative prefixes where tools keep tokens, keys and password
// stores; a directory hides everything beneath it.
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

// credentialEnvPaths relocate a credential store or name a socket; KUBECONFIG is a list.
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

// runtimeSocketNames are keychain, agent and bus sockets, hidden because a connection to one
// can fetch a secret without reading any file.
var runtimeSocketNames = []string{"bus", "keyring", "gnupg", "gcr", "ssh-agent.socket"}

// runnerRuntimeDir matches RuntimeDirectory= in deploy/systemd/flynn-runner.service.
const runnerRuntimeDir = "/run/flynn"

// credentialPaths takes the environment as a function so it can compute the set for another
// user, and a test needs no real home.
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
		runtime = filepath.Join(filepath.FromSlash("/run/user"), strconv.Itoa(uid))
	}
	if runtime != "" {
		for _, name := range runtimeSocketNames {
			paths = append(paths, filepath.Join(runtime, name))
		}
	}
	return paths
}

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

// declareShellPosture reads the posture from sb itself, so the sealed record states what
// the launching sandbox enforces. A failed write fails the assembly, so no run starts under
// a relaxation it cannot account for.
func declareShellPosture(ctx context.Context, log spine.Log, stream string, sb *sandbox.Local, reason, networkReason string) error {
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
	if network == "open" && networkReason != "" {
		payload["network_reason"] = networkReason
	}
	if len(hidden) > 0 {
		payload["hidden"] = hidden
	} else if reason != "" {
		payload["credentials_reason"] = reason
	}
	_, err := log.Append(ctx, spine.AppendInput{
		Stream:  stream,
		Type:    ShellPostureEventType,
		Actor:   spine.ActorSystem,
		Payload: payload,
	})
	return err
}
