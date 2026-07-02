package playbook

import (
	"context"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/flow"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/secret"
)

// CredentialSink implements flow.CredentialSink: it resolves a secret by reference through
// the secret source (the origin) and materializes it into a hosting provider's secret store
// by driving the provider's CLI in the sandbox. The value is read from the source and
// delivered to the provider on standard input, so it never appears on a command line, in
// the flow's data, in the step output, or in a log. It is the provision-direction
// counterpart of secret.Source: Source resolves a credential for the agent to use; this
// sink hands a credential to a workload the agent is standing up.
type CredentialSink struct {
	src secret.Source
	sb  sandbox.Sandbox
}

// NewCredentialSink builds a sink over the secret source that resolves references and the
// sandbox that runs the provider CLI. With either missing, every materialization fails
// closed.
func NewCredentialSink(src secret.Source, sb sandbox.Sandbox) *CredentialSink {
	return &CredentialSink{src: src, sb: sb}
}

// Put resolves the secret named by ref and materializes it into the named sink's target.
// The value is held only as a secret.Text and delivered inside the adapter; it is never
// returned, logged, or placed on a command line.
func (c *CredentialSink) Put(ctx context.Context, sink, ref string, target map[string]string) error {
	if c.src == nil || c.sb == nil {
		return fault.New(fault.Terminal, "playbook_sink_unconfigured", "playbook: credential sink has no secret source or sandbox")
	}
	val, err := c.src.Lookup(ctx, ref)
	if err != nil {
		// ref is a reference name, never the value; secret.Text would redact a value anyway.
		return fmt.Errorf("playbook: resolve secret %q: %w", ref, err)
	}
	defer val.Destroy()

	switch sink {
	case "fly":
		return c.flyImport(ctx, val, target)
	default:
		return fault.New(fault.Terminal, "playbook_unknown_sink", "playbook: unknown credential sink "+sink)
	}
}

// flyImport stages a single secret into a Fly app via `flyctl secrets import`, which reads
// KEY=value lines from standard input. The value travels on the pipe, so it is never on the
// command line. --stage records the secret without triggering a deploy, so the caller
// controls when the workload restarts. target needs app (the Fly app), key (the
// environment variable name to set), and cli (the resolved flyctl path).
func (c *CredentialSink) flyImport(ctx context.Context, val secret.Text, target map[string]string) error {
	app, key, cli := target["app"], target["key"], target["cli"]
	if app == "" || key == "" || cli == "" {
		return fault.New(fault.Terminal, "playbook_fly_sink_target",
			"playbook: fly secret sink needs target.app, target.key, and target.cli")
	}
	// KEY=value on stdin. fly secrets import echoes only the key names it staged, never
	// the values, so the command output is safe to surface.
	kv := []byte(key + "=" + val.Expose() + "\n")
	line := cli + " secrets import --stage --app " + app
	res, err := c.sb.Exec(ctx, sandbox.Command{Line: line, Stdin: kv})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fault.New(fault.Terminal, "playbook_fly_sink_failed",
			fmt.Sprintf("playbook: staging secret %q into fly app %q exited %d: %s",
				key, app, res.ExitCode, strings.TrimSpace(res.Output)))
	}
	return nil
}

var _ flow.CredentialSink = (*CredentialSink)(nil)
