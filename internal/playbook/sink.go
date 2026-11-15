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

// CredentialSink resolves a secret by reference and stages it into a hosting
// provider's secret store by running the provider's CLI in the sandbox. The value
// goes on standard input, so it never appears on a command line, in flow data, in
// step output or in a log.
type CredentialSink struct {
	src secret.Source
	sb  sandbox.Sandbox
}

// NewCredentialSink returns a CredentialSink. With src or sb nil, every Put fails
// closed.
func NewCredentialSink(src secret.Source, sb sandbox.Sandbox) *CredentialSink {
	return &CredentialSink{src: src, sb: sb}
}

// Put resolves ref and stages the value into sink's target. Only "fly" is
// supported.
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

// flyImport runs `flyctl secrets import`, which reads KEY=value from stdin. --stage
// skips the deploy, so the caller decides when the workload restarts. target needs
// app, key (the environment variable) and cli (the flyctl path).
func (c *CredentialSink) flyImport(ctx context.Context, val secret.Text, target map[string]string) error {
	app, key, cli := target["app"], target["key"], target["cli"]
	if app == "" || key == "" || cli == "" {
		return fault.New(fault.Terminal, "playbook_fly_sink_target",
			"playbook: fly secret sink needs target.app, target.key, and target.cli")
	}
	// flyctl echoes only the key names, so the output is safe to surface.
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
