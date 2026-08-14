package externagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/fault"
)

// bundledAdapters is every adapter this repository ships. The lockdown tests run over the
// list rather than over a named pair, so a third adapter is held to the contract by being
// added here, which is the one step its author cannot skip and still have it selectable.
func bundledAdapters() []Adapter {
	return []Adapter{NewClaude("claude", nil), NewCodex("codex", nil)}
}

// TestEveryBundledAdapterDeclaresAStrippedLockdown is the invariant the contract exists
// for: no adapter may leave a native write surface, a native command surface, or the
// operator's own configuration in place. An adapter that declares nothing fails here on
// the zero value, so forgetting the declaration is the same as failing it.
func TestEveryBundledAdapterDeclaresAStrippedLockdown(t *testing.T) {
	for _, a := range bundledAdapters() {
		t.Run(a.Name(), func(t *testing.T) {
			l := a.Lockdown()
			if why := l.Refusal(a.Name()); why != "" {
				t.Fatalf("%s must run under a stripped lockdown: %s", a.Name(), why)
			}
			if !l.Writes.Stripped() || !l.Commands.Stripped() || !l.HostConfig.Stripped() {
				t.Fatalf("%s lockdown = %+v, want every class stripped", a.Name(), l)
			}
		})
	}
}

// TestClaudeLockdownMatchesTheArgvItBuilds proves the declaration is a description of the
// invocation rather than a second place to state an intention. claude declares its writes
// and commands denied, so the effectors must be named on the denial list, and the
// permission mode must be one that neither prompts nor auto-approves: bypassPermissions
// would leave the model holding every tool the deny list did not name.
func TestClaudeLockdownMatchesTheArgvItBuilds(t *testing.T) {
	c := NewClaude("claude", nil)
	inv, err := c.Command(Episode{Input: "hello", Bridge: Bridge{Name: "flynn", URL: "http://127.0.0.1:1/mcp"}})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	args := strings.Join(inv.Args, " ")

	if c.Lockdown().Writes != StripDenied || c.Lockdown().Commands != StripDenied {
		t.Fatalf("claude lockdown = %+v, want writes and commands denied", c.Lockdown())
	}
	for _, tool := range []string{"Bash", "Edit", "Write", "NotebookEdit"} {
		if !hasArg(inv.Args, tool) {
			t.Errorf("claude declares its native effectors denied but %q is not on --disallowedTools: %s", tool, args)
		}
	}
	if !hasArg(inv.Args, "--disallowedTools") {
		t.Errorf("claude declares denial but passes no --disallowedTools: %s", args)
	}
	if strings.Contains(args, "bypassPermissions") {
		t.Errorf("claude must not run under bypassPermissions, which auto-approves what the deny list did not name: %s", args)
	}

	// The config half: strict MCP config is what keeps the operator's own servers out of
	// the child, so the declaration that host config is denied rests on it.
	if c.Lockdown().HostConfig != StripDenied {
		t.Fatalf("claude host config = %v, want denied", c.Lockdown().HostConfig)
	}
	if !hasArg(inv.Args, "--strict-mcp-config") {
		t.Errorf("claude declares the operator's configuration denied but does not pass --strict-mcp-config: %s", args)
	}
}

// TestCodexLockdownMatchesTheArgvItBuilds proves the same for the provider that reaches
// the other verdict from the same contract. codex has no flag that removes its shell or
// patch tools, so it declares containment, and containment means the read-only sandbox
// with its approval path denied must actually be on the command line: with either missing,
// the tools codex still offers the model would work.
func TestCodexLockdownMatchesTheArgvItBuilds(t *testing.T) {
	c := NewCodex("codex", nil)
	inv, err := c.Command(Episode{Input: "hello", Bridge: Bridge{Name: "flynn", URL: "http://127.0.0.1:1/mcp"}})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	args := strings.Join(inv.Args, " ")

	l := c.Lockdown()
	if l.Writes != StripContained || l.Commands != StripContained {
		t.Fatalf("codex lockdown = %+v, want writes and commands contained", l)
	}
	if !strings.Contains(args, "--sandbox read-only") {
		t.Errorf("codex declares containment but does not pin the read-only sandbox: %s", args)
	}
	if !strings.Contains(args, `approval_policy="never"`) {
		t.Errorf("codex declares containment but leaves an approval path open: %s", args)
	}
}

// TestRunnerRefusesAProviderItCannotStrip is the refusal the invariant turns on. A
// provider whose writes cannot be taken away is not graded, not run, and not downgraded:
// the episode ends before anything is spawned, with a terminal fault naming what could not
// be stripped.
func TestRunnerRefusesAProviderItCannotStrip(t *testing.T) {
	workdir := t.TempDir()
	srv, ctx := governedBridge(t, workdir, capability.AllowAll())

	spawner := fakeSpawner{start: func(context.Context, Episode, Invocation) (Process, error) {
		return nil, errors.New("a provider that cannot be stripped must never be spawned")
	}}
	adapter := lockdownAdapter{
		Codex: NewCodex("codex", nil),
		lockdown: Lockdown{
			Writes:     StripImpossible,
			Commands:   StripContained,
			HostConfig: StripContained,
			Reason:     "this build offers no way to deny or contain its editor",
		},
	}

	_, err := NewRunner(adapter, srv, spawner, nil).Run(ctx, Episode{Input: "hi", Workdir: workdir})
	if err == nil {
		t.Fatal("an unstrippable provider must refuse the episode")
	}
	if got := fault.Classify(err); got != fault.Terminal {
		t.Errorf("refusal class = %v, want terminal", got)
	}
	msg := err.Error()
	for _, want := range []string{"codex", "native writes", "impossible", "offers no way to deny"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q must name %q", msg, want)
		}
	}
}

// TestRunnerRefusesAnUndeclaredLockdown proves the zero value fails closed. An adapter
// that says nothing about its native surface is refused exactly like one that says it
// cannot strip it, so an omission cannot pass for a lockdown.
func TestRunnerRefusesAnUndeclaredLockdown(t *testing.T) {
	workdir := t.TempDir()
	srv, ctx := governedBridge(t, workdir, capability.AllowAll())

	spawner := fakeSpawner{start: func(context.Context, Episode, Invocation) (Process, error) {
		return nil, errors.New("an undeclared lockdown must never be spawned")
	}}
	adapter := lockdownAdapter{Codex: NewCodex("codex", nil)}

	_, err := NewRunner(adapter, srv, spawner, nil).Run(ctx, Episode{Input: "hi", Workdir: workdir})
	if err == nil {
		t.Fatal("an adapter that declares no lockdown must refuse the episode")
	}
	if !strings.Contains(err.Error(), "undeclared") {
		t.Errorf("refusal %q must say the posture was undeclared", err)
	}
}

// TestRefusalNamesEveryUnstrippedClass proves the message is the whole answer. An operator
// told about one gap at a time closes it, re-runs, and meets the next, which is the same
// integration failure spread over three attempts.
func TestRefusalNamesEveryUnstrippedClass(t *testing.T) {
	l := Lockdown{Writes: StripImpossible, Commands: StripUndeclared, HostConfig: StripImpossible}
	msg := l.Refusal("someagent")
	for _, want := range []string{"someagent", "native writes", "native commands", "the operator's own configuration"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q must name %q", msg, want)
		}
	}
	if got := (Lockdown{Writes: StripDenied, Commands: StripContained, HostConfig: StripDenied}).Refusal("someagent"); got != "" {
		t.Errorf("a stripped lockdown must not refuse, got %q", got)
	}
}

// TestStripNames pins the words a refusal and the record are read with.
func TestStripNames(t *testing.T) {
	for _, tc := range []struct {
		s    Strip
		want string
	}{
		{StripUndeclared, "undeclared"},
		{StripDenied, "denied"},
		{StripContained, "contained"},
		{StripImpossible, "impossible"},
		{Strip(99), "unknown"},
	} {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("Strip(%d).String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// lockdownAdapter is an Adapter whose declared lockdown is scripted, so the runner's
// refusal is exercised without a provider that really cannot be stripped. Everything else
// falls through to the codex adapter.
type lockdownAdapter struct {
	*Codex
	lockdown Lockdown
}

func (a lockdownAdapter) Lockdown() Lockdown { return a.lockdown }

// hasArg reports whether args carries s as a whole argument, so a check for a tool name
// cannot be satisfied by another argument that merely contains it.
func hasArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}
