package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/flow"
	"github.com/ionalpha/flynn/internal/playbook"
	"github.com/ionalpha/flynn/internal/service"
)

func TestIndentLines(t *testing.T) {
	if got := indentLines("one\ntwo", "  "); got != "  one\n  two" {
		t.Errorf("indentLines = %q", got)
	}
	if got := indentLines("", ">"); got != ">" {
		t.Errorf("an empty string still gets the prefix, got %q", got)
	}
}

// TestTerminalProgressReportsEachOp locks what an operator sees while a long procedure
// runs: the command as it starts, its output and exit status as it ends, and, for the
// secret op, only the sink and reference name (never a value).
func TestTerminalProgressReportsEachOp(t *testing.T) {
	cases := []struct {
		name  string
		event flow.StepEvent
		want  []string
	}{
		{"exec begin", flow.StepEvent{Phase: flow.StepBegin, Op: flow.OpExec, Detail: "fly deploy"}, []string{"$ fly deploy"}},
		{"exec end", flow.StepEvent{Phase: flow.StepEnd, Op: flow.OpExec, Detail: "fly deploy", Output: "up\nrunning", ExitCode: 0}, []string{"  up\n  running", "[exit 0]"}},
		{"exec end, no output", flow.StepEvent{Phase: flow.StepEnd, Op: flow.OpExec, ExitCode: 3}, []string{"[exit 3]"}},
		{"dependency begin", flow.StepEvent{Phase: flow.StepBegin, Op: flow.OpDependency, Detail: "flyctl"}, []string{"ensuring flyctl is available"}},
		{"dependency end", flow.StepEvent{Phase: flow.StepEnd, Op: flow.OpDependency, Detail: "flyctl"}, []string{"ready"}},
		{"secret begin", flow.StepEvent{Phase: flow.StepBegin, Op: flow.OpSecret, Detail: "fly:API_KEY"}, []string{"materializing secret fly:API_KEY"}},
		{"secret end", flow.StepEvent{Phase: flow.StepEnd, Op: flow.OpSecret, Detail: "fly:API_KEY"}, []string{"staged"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			terminalProgress{w: &out}.Step(c.event)
			for _, want := range c.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("progress = %q, want it to contain %q", out.String(), want)
				}
			}
		})
	}
}

// TestTerminalProgressStaysQuietOnFailure checks a failed step prints nothing: the run's
// error carries the command output, so it is reported once, by the caller.
func TestTerminalProgressStaysQuietOnFailure(t *testing.T) {
	var out bytes.Buffer
	p := terminalProgress{w: &out}
	p.Step(flow.StepEvent{Phase: flow.StepEnd, Op: flow.OpExec, Detail: "fly deploy", Output: "boom", Err: errors.New("exit 1")})
	// An op with nothing to show is silent too.
	p.Step(flow.StepEvent{Phase: flow.StepBegin, Op: flow.Op("set")})
	p.Step(flow.StepEvent{Phase: flow.StepEnd, Op: flow.Op("set")})
	if out.Len() != 0 {
		t.Errorf("a failed or silent step must print nothing, got %q", out.String())
	}
}

// TestTerminalConfirmer covers the answers a confirmation step can get: approval, a
// decline, and no input at all, which must fail closed rather than proceed silently.
func TestTerminalConfirmer(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
		code    string
	}{
		{"empty line approves", "\n", false, ""},
		{"any word approves", "yes\n", false, ""},
		{"n declines", "n\n", true, "playbook_confirm_declined"},
		{"no declines, case-insensitively", "  NO  \n", true, "playbook_confirm_declined"},
		{"no input fails closed", "", true, "playbook_confirm_noinput"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var prompt bytes.Buffer
			conf := terminalConfirmer{in: strings.NewReader(c.in), out: &prompt}
			err := conf.Confirm(context.Background(), "deploy to production?")
			if !strings.Contains(prompt.String(), "deploy to production?") {
				t.Errorf("the step's message must be shown, got %q", prompt.String())
			}
			if !c.wantErr {
				if err != nil {
					t.Fatalf("expected approval, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected the step to be refused")
			}
			var f *fault.Error
			if !errors.As(err, &f) || f.Code != c.code {
				t.Fatalf("error = %v, want fault code %s", err, c.code)
			}
			if f.Class != fault.Cancelled {
				t.Errorf("a refusal must be classified cancelled, got %v", f.Class)
			}
		})
	}
}

// TestFillDerivedNamesLeavesNonFlyAndExplicitNamesAlone checks the derivation only applies
// where a globally-unique provider name is actually needed, and that an operator's explicit
// choice always wins.
func TestFillDerivedNamesLeavesNonFlyAndExplicitNamesAlone(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	cases := []struct {
		name string
		pb   playbook.Playbook
		cfg  map[string]any
	}{
		{"no service block", playbook.Playbook{Name: "x"}, map[string]any{}},
		{"another provider", playbook.Playbook{Spec: playbook.Spec{Service: &playbook.ServiceBlock{Provider: "cloudflare"}}}, map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := fillDerivedNames(ctx, dataDir, c.pb, c.cfg); err != nil {
				t.Fatalf("fillDerivedNames: %v", err)
			}
			if _, ok := c.cfg["app"]; ok {
				t.Errorf("no name should have been derived, got %v", c.cfg)
			}
		})
	}

	fly := playbook.Playbook{Spec: playbook.Spec{Service: &playbook.ServiceBlock{Provider: "fly"}}}
	cfg := map[string]any{"app": "my-own-app"}
	if err := fillDerivedNames(ctx, dataDir, fly, cfg); err != nil {
		t.Fatalf("fillDerivedNames: %v", err)
	}
	if cfg["app"] != "my-own-app" {
		t.Errorf("an explicit app name must win, got %v", cfg["app"])
	}
}

// TestFillDerivedNamesIsStableForOneIdentity checks the point of the derivation: with an
// instance identity in scope, a blank app input resolves to the same provider-valid name on
// every redeploy.
func TestFillDerivedNamesIsStableForOneIdentity(t *testing.T) {
	t.Setenv("FLYNN_VAULT_FILE", "1")
	t.Setenv("FLYNN_VAULT_PASSPHRASE", "pw")
	ctx := context.Background()
	dataDir := t.TempDir()
	fly := playbook.Playbook{Spec: playbook.Spec{Service: &playbook.ServiceBlock{Provider: "fly"}}}

	first := map[string]any{}
	if err := fillDerivedNames(ctx, dataDir, fly, first); err != nil {
		t.Fatalf("fillDerivedNames: %v", err)
	}
	name, _ := first["app"].(string)
	if name == "" {
		t.Fatal("a blank app input must be filled in")
	}
	if len(name) > flyAppMaxLen {
		t.Errorf("derived app name %q is longer than the %d-char DNS budget", name, flyAppMaxLen)
	}
	if strings.ToLower(name) != name || strings.ContainsAny(name, "._ /") {
		t.Errorf("derived app name %q is not a DNS label", name)
	}

	// A blank input under the same identity resolves to the same name; that stability is
	// what keeps a redeploy from creating a second app.
	second := map[string]any{"app": "   "}
	if err := fillDerivedNames(ctx, dataDir, fly, second); err != nil {
		t.Fatalf("fillDerivedNames: %v", err)
	}
	if second["app"] != name {
		t.Errorf("derived name is not stable: %v then %v", name, second["app"])
	}

	// A different instance derives a different name, so two instances never collide.
	other := map[string]any{}
	if err := fillDerivedNames(ctx, t.TempDir(), fly, other); err != nil {
		t.Fatalf("fillDerivedNames: %v", err)
	}
	if other["app"] == name {
		t.Errorf("two instances derived the same app name %v", name)
	}
}

func TestRunPlaybookRejectsAnUnknownSubcommand(t *testing.T) {
	err := runPlaybook([]string{"deploy"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("expected an unknown-subcommand error, got %v", err)
	}
}

// TestPlaybookListShowsTheCatalog checks the bare command lists the synced catalog with the
// service each playbook registers.
func TestPlaybookListShowsTheCatalog(t *testing.T) {
	var out bytes.Buffer
	if err := playbookList(context.Background(), t.TempDir(), &out); err != nil {
		t.Fatalf("playbookList: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "NAME") || !strings.Contains(got, "REGISTERS") || !strings.Contains(got, "DESCRIPTION") {
		t.Fatalf("the listing must be a labelled table, got:\n%s", got)
	}
	// The embedded catalog ships at least one playbook, each on its own row under the header.
	if lines := strings.Count(strings.TrimSpace(got), "\n"); lines < 1 {
		t.Fatalf("the catalog should list at least one playbook, got:\n%s", got)
	}
}

// TestRunPlaybookDefaultsToListing checks a bare `flynn playbook` lists rather than erroring.
func TestRunPlaybookDefaultsToListing(t *testing.T) {
	if err := runPlaybook(nil, t.TempDir()); err != nil {
		t.Fatalf("a bare playbook command must list the catalog, got %v", err)
	}
}

func TestPlaybookRunArgumentErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no name", nil, "usage: flynn playbook run"},
		{"bad json input", []string{"fly-deploy", "{not json"}, "input is not valid JSON"},
		{"unknown playbook", []string{"no-such-playbook"}, "unknown playbook"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := playbookRun(ctx, t.TempDir(), c.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("playbookRun(%v) = %v, want an error containing %q", c.args, err, c.want)
			}
		})
	}
}

// TestPrintPlaybookResult covers what an operator is told when a playbook finishes: the
// registered service and its URL when the playbook produced one, the bare completion line
// when it did not, and the flow's output either way.
func TestPrintPlaybookResult(t *testing.T) {
	cases := []struct {
		name string
		res  playbook.Result
		want []string
		omit string
	}{
		{
			name: "service with a url",
			res: playbook.Result{
				Output:  map[string]any{"app": "flynn-agent-abc"},
				Service: &service.Service{Name: "flynn-agent-abc", Spec: service.Spec{URL: "https://flynn-agent-abc.fly.dev"}},
			},
			want: []string{`Registered service "flynn-agent-abc"`, "-> https://flynn-agent-abc.fly.dev", `"app": "flynn-agent-abc"`},
		},
		{
			name: "service without a url",
			res:  playbook.Result{Service: &service.Service{Name: "svc"}},
			want: []string{`Registered service "svc"`},
			omit: "->",
		},
		{
			name: "no service",
			res:  playbook.Result{Output: map[string]any{"ok": true}},
			want: []string{`Playbook "fly-deploy" done.`, "Result:", `"ok": true`},
			omit: "Registered service",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			printPlaybookResult(&out, "fly-deploy", c.res)
			for _, want := range c.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("result report %q is missing %q", out.String(), want)
				}
			}
			if c.omit != "" && strings.Contains(out.String(), c.omit) {
				t.Errorf("result report %q must not contain %q", out.String(), c.omit)
			}
		})
	}
}

// TestOpenPlaybookRuntimeSyncsTheCatalogs checks the wiring the run path depends on: both
// the playbook and the dependency catalogs are present in the store the runner reads.
func TestOpenPlaybookRuntimeSyncsTheCatalogs(t *testing.T) {
	ctx := context.Background()
	rt, err := openPlaybookRuntime(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("openPlaybookRuntime: %v", err)
	}
	defer func() { _ = rt.closer() }()

	pbs, err := rt.store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pbs) == 0 {
		t.Fatal("the playbook catalog was not synced into the store")
	}
	if rt.runner == nil {
		t.Fatal("the runner must be wired with its effect ports")
	}
	// A named playbook resolves, and an unknown one is reported as not found rather than
	// as a store failure.
	if _, err := rt.store.Get(ctx, pbs[0].Name); err != nil {
		t.Errorf("Get(%q): %v", pbs[0].Name, err)
	}
	if _, err := rt.store.Get(ctx, "no-such-playbook"); !errors.Is(err, playbook.ErrNotFound) {
		t.Errorf("Get of an unknown playbook = %v, want ErrNotFound", err)
	}
}
