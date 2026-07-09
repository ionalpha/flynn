package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/externagent"
)

// externalAgentBackends are the CLI harness names selectable through the --model
// spec. A spec whose scheme (the part before the first colon) names one of these is
// not a hosted model to resolve a credential for: it routes the run to that external
// agent CLI driver, and the rest of the spec is the model the CLI itself should
// drive. Adding a second harness is one entry here plus a case in newExternalAgent,
// not a new resolution path.
var externalAgentBackends = map[string]bool{"codex": true}

// externalAgentSpec splits a --model spec into an external-agent backend name and the
// model string to hand the CLI, reporting whether the spec names an external agent at
// all. A bare name selects the CLI with its own default model; a name:model spec pins
// the model. "codex" -> ("codex", "", true); "codex:gpt-5-codex" -> ("codex",
// "gpt-5-codex", true); "anthropic:claude-..." -> ("", "", false).
func externalAgentSpec(modelSpec string) (name, model string, ok bool) {
	name, model, _ = strings.Cut(modelSpec, ":")
	if !externalAgentBackends[name] {
		return "", "", false
	}
	return name, model, true
}

// externAgent is a resolved external-agent backend: the driver that builds the
// episode loop and the model string the CLI drives, ready to plug into a run in place
// of a native llm.Model.
type externAgent struct {
	model  string
	driver driver.Driver
}

// tierTallier is an external-agent driver that tallies the provenance tiers of the
// events its episodes projected, how the harness chose its tools, and which parts of the
// session contract it ignored. The driver port itself stays free of provenance (a native
// loop has no tiers to report and no contract to drift from), so the host asks for the
// capability rather than requiring it.
type tierTallier interface {
	Tiers() map[externagent.Tier]int
	Steering() externagent.Steering
	Drift() map[string]int
}

// observedProvenance is what the host can say about an external-harness run once it has
// finished: the harness's own account of itself, and how far it strayed from the contract
// the run gave it. A driver that reports none of this yields a bare declaration, which
// still names the run as externally driven.
func observedProvenance(ea *externAgent) externalProvenance {
	d := externalProvenance{harness: ea.driver.Name()}
	t, ok := ea.driver.(tierTallier)
	if !ok {
		return d
	}
	d.attested = t.Tiers()[externagent.TierAttested]
	d.nativeRate = t.Steering().NativeRate()
	d.drift = t.Drift()
	return d
}

// codexAllowedHosts are the destination names a codex episode's confined child may
// reach at the egress waist: the OpenAI API and ChatGPT subscription-auth endpoints
// codex talks to. Everything else is denied, so the external harness's own provider
// channel is contained to these names even though its inner traffic is unobserved. A
// name that resolves to a private or rebinding address is still refused by the address
// gate.
var codexAllowedHosts = []string{
	"api.openai.com",
	"auth.openai.com",
	"chatgpt.com",
	".chatgpt.com",
}

// codexAuthDir is the codex CLI's credential and config home, granted read to the
// confined child so an auth-status probe and a live episode can see the subscription
// token without it being copied into the workspace. CODEX_HOME overrides the default
// (~/.codex), matching the CLI's own resolution. Empty when no home directory is
// resolvable, which grants no extra read.
func codexAuthDir() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// externalProbeTimeout caps a single detection probe (a version or auth-status check)
// so a hung CLI cannot stall onboarding.
const externalProbeTimeout = 15 * time.Second

// externalSpawner builds the production sandbox spawner for the named backend: the
// external CLI runs as an untrusted subprocess under the kernel-confined floor, with a
// deny-all-except-provider egress gate and a read grant for its own auth home. One
// place builds the confinement envelope so detection and the live episode run under
// the same boundary and provider allowlist, and cannot drift.
func externalSpawner(name string) (externagent.Spawner, error) {
	switch name {
	case "codex":
		return externagent.NewSandboxSpawner(externagent.SandboxConfig{
			AllowedHosts: codexAllowedHosts,
			AuthDir:      codexAuthDir(),
			ProbeTimeout: externalProbeTimeout,
		}), nil
	default:
		return nil, fmt.Errorf("unknown external agent backend %q", name)
	}
}

// externalAdapter builds the CLI adapter for the named backend over spawner. The
// adapter describes how to detect the CLI and drive one episode; the codex adapter is
// the first.
func externalAdapter(name string, spawner externagent.Spawner) (externagent.Adapter, error) {
	switch name {
	case "codex":
		return externagent.NewCodex("", spawner), nil
	default:
		return nil, fmt.Errorf("unknown external agent backend %q", name)
	}
}

// newExternalAgent builds the driver for the named external agent backend, with
// workdir as the directory the episode operates in. It shares one spawner between the
// adapter and the driver so probes and episodes run under the same confinement. It
// does not probe the CLI; resolveExternalAgent does that first.
func newExternalAgent(name, workdir string) (*externAgent, error) {
	spawner, err := externalSpawner(name)
	if err != nil {
		return nil, err
	}
	adapter, err := externalAdapter(name, spawner)
	if err != nil {
		return nil, err
	}
	return &externAgent{
		driver: externagent.NewDriver(adapter, spawner, workdir),
	}, nil
}

// resolveExternalAgent detects the external agent CLI and, when it is installed,
// logged in, and new enough to be constrained, returns the driver to run it under. It
// mirrors resolveModelOrOnboard's shape for a native provider: a hard refusal (a CLI
// too old to lock down, a missing containment knob) is terminal, and a recoverable
// onboarding gap (not installed, or not logged in) surfaces the CLI's own actionable
// reason ("run `codex login`", "install it") rather than a raw error, so the user
// knows the next step. No API key is ever asked for: an external agent authenticates
// on its own subscription.
func resolveExternalAgent(ctx context.Context, name, model, workdir string) (*externAgent, error) {
	spawner, err := externalSpawner(name)
	if err != nil {
		return nil, err
	}
	adapter, err := externalAdapter(name, spawner)
	if err != nil {
		return nil, err
	}
	r, err := adapter.Detect(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect %s: %w", name, err)
	}
	if rerr := readinessError(name, r); rerr != nil {
		return nil, rerr
	}

	ea, err := newExternalAgent(name, workdir)
	if err != nil {
		return nil, err
	}
	ea.model = model
	fmt.Fprintf(os.Stderr, "Using %s %s on your subscription. Its tool calls are governed and recorded through the flynn bridge; its inner reasoning is recorded as an unobserved-but-contained gap.\n",
		name, r.Version)
	return ea, nil
}

// readinessError turns a not-ready Readiness into the actionable error to surface to
// the user, or nil when the CLI is ready to run. A hard refusal (Refuse) is terminal:
// the CLI cannot be constrained to route its effects through the bridge, so the run
// must not start it with unattested native effects. An onboarding gap (not installed,
// or not logged in) surfaces the adapter's own actionable Reason ("run `codex login`",
// "install it") rather than a raw error, so the user knows the next step; no API key
// is ever requested, since an external agent authenticates on its own subscription.
func readinessError(name string, r externagent.Readiness) error {
	switch {
	case r.Refuse:
		return fmt.Errorf("%s cannot be governed as a backend: %s", name, r.Reason)
	case !r.Ready():
		return fmt.Errorf("%s", r.Reason)
	default:
		return nil
	}
}
