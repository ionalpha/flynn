package playbook

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/flow"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/service"
)

// stores builds a memory store registered for both the Playbook and Service kinds, with a
// facade over each, so a runner test can register a playbook and observe the service it
// produces.
func stores(t *testing.T) (*Store, *service.Store) {
	t.Helper()
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register playbook kind: %v", err)
	}
	if err := service.RegisterKind(reg); err != nil {
		t.Fatalf("register service kind: %v", err)
	}
	rs := resource.NewMemory(reg)
	return NewStore(rs), service.NewStore(rs)
}

// okExecer returns success for any command; used by flows that do not exercise exec.
type okExecer struct{}

func (okExecer) Exec(context.Context, flow.ExecRequest) (flow.ExecResult, error) {
	return flow.ExecResult{ExitCode: 0}, nil
}

// flyExecer models flyctl for the Fly playbook: it is not authenticated until "auth login"
// runs (simulating a completed browser login), and the app does not exist until "apps
// create" runs. It records every command so a test can assert the control flow.
type flyExecer struct {
	authed    bool // becomes true once auth login runs, unless loginNoOp
	loginNoOp bool // login "runs" but does not authenticate (browser not completed)
	appExists bool
	loginRuns int
	seen      []string
}

func (e *flyExecer) Exec(_ context.Context, req flow.ExecRequest) (flow.ExecResult, error) {
	c := req.Command
	e.seen = append(e.seen, c)
	switch {
	case strings.Contains(c, "auth login"):
		e.loginRuns++
		if !e.loginNoOp {
			e.authed = true
		}
		return flow.ExecResult{ExitCode: 0, Output: "logged in"}, nil
	case strings.Contains(c, "auth whoami"):
		if e.authed {
			return flow.ExecResult{ExitCode: 0, Output: "ops@example.com"}, nil
		}
		return flow.ExecResult{ExitCode: 1, Output: "not logged in"}, nil
	case strings.Contains(c, "apps list"):
		if e.appExists {
			return flow.ExecResult{ExitCode: 0, Output: "myapp\nother\n"}, nil
		}
		return flow.ExecResult{ExitCode: 0, Output: "other\n"}, nil
	case strings.Contains(c, "apps create"):
		e.appExists = true
		return flow.ExecResult{ExitCode: 0, Output: "created"}, nil
	default: // deploy, status, ...
		return flow.ExecResult{ExitCode: 0, Output: "ok"}, nil
	}
}

func (e *flyExecer) ran(substr string) bool {
	for _, c := range e.seen {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// fixedResolver resolves every dependency to one path.
type fixedResolver struct{ path string }

func (r fixedResolver) Resolve(context.Context, string) (string, error) { return r.path, nil }

// autoConfirm approves every confirmation; declineConfirm rejects it. They stand in for an
// operator pressing Enter or cancelling at a confirm step.
type autoConfirm struct{}

func (autoConfirm) Confirm(context.Context, string) error { return nil }

type declineConfirm struct{}

func (declineConfirm) Confirm(context.Context, string) error {
	return errors.New("cancelled by operator")
}

func putPlaybook(t *testing.T, s *Store, name string, spec Spec) Playbook {
	t.Helper()
	pb, err := s.Put(context.Background(), name, spec)
	if err != nil {
		t.Fatalf("put playbook: %v", err)
	}
	return pb
}

func flyPlaybook(t *testing.T, ps *Store) Playbook {
	t.Helper()
	if _, err := Sync(context.Background(), ps); err != nil {
		t.Fatalf("sync: %v", err)
	}
	pb, err := ps.Get(context.Background(), "fly-app")
	if err != nil {
		t.Fatalf("get fly-app: %v", err)
	}
	return pb
}

// TestRunnerRegistersService proves a playbook whose flow returns a workload registers a
// supervised Service classified by the playbook's service block.
func TestRunnerRegistersService(t *testing.T) {
	ps, svc := stores(t)
	pb := putPlaybook(t, ps, "demo", Spec{
		Service: &ServiceBlock{Provider: "fly", Target: service.TargetContainer},
		Flow: json.RawMessage(`{"steps":[
			{"op":"return","return":{"value":{
				"name":"{{config.app}}","url":"https://{{config.app}}.fly.dev","address":{"app":"{{config.app}}"}
			}}}
		]}`),
	})
	r := NewRunner(okExecer{}, fixedResolver{path: "/deps/flyctl"}, svc)

	res, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Service == nil || res.Service.Name != "myapp" {
		t.Fatalf("service not registered: %+v", res.Service)
	}
	got, err := svc.Get(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("service not stored: %v", err)
	}
	if got.Spec.Provider != "fly" || got.Spec.Target != service.TargetContainer ||
		got.Spec.URL != "https://myapp.fly.dev" || got.Spec.DesiredState != service.StateRunning ||
		got.Spec.Address["app"] != "myapp" {
		t.Fatalf("service spec wrong: %+v", got.Spec)
	}
}

// TestRunnerTriggersLoginWhenUnauthed proves the Fly playbook runs the login flow (which
// opens the browser) when the operator is not authenticated, then proceeds to deploy and
// register the service.
func TestRunnerTriggersLoginWhenUnauthed(t *testing.T) {
	ps, svc := stores(t)
	pb := flyPlaybook(t, ps)
	ex := &flyExecer{authed: false}
	r := NewRunner(ex, fixedResolver{path: "/deps/flyctl"}, svc, WithConfirmer(autoConfirm{}))

	res, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if ex.loginRuns != 1 {
		t.Fatalf("expected login to run exactly once, ran %d", ex.loginRuns)
	}
	if !ex.ran("apps create myapp") || !ex.ran("deploy") {
		t.Fatal("expected create + deploy after login")
	}
	if res.Service == nil || res.Service.Spec.URL != "https://myapp.fly.dev" {
		t.Fatalf("service not registered: %+v", res.Service)
	}
}

// TestRunnerSkipsLoginWhenAuthed proves an already-authenticated run does not open the
// browser: the login step is skipped.
func TestRunnerSkipsLoginWhenAuthed(t *testing.T) {
	ps, svc := stores(t)
	pb := flyPlaybook(t, ps)
	ex := &flyExecer{authed: true}
	r := NewRunner(ex, fixedResolver{path: "/deps/flyctl"}, svc)

	if _, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if ex.loginRuns != 0 {
		t.Fatalf("an authenticated run must not run login, ran %d", ex.loginRuns)
	}
}

// TestRunnerSkipsCreateWhenAppExists proves the playbook is level-triggered: an app that
// already exists is not created again.
func TestRunnerSkipsCreateWhenAppExists(t *testing.T) {
	ps, svc := stores(t)
	pb := flyPlaybook(t, ps)
	ex := &flyExecer{authed: true, appExists: true}
	r := NewRunner(ex, fixedResolver{path: "/deps/flyctl"}, svc)

	if _, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if ex.ran("apps create") {
		t.Fatal("an existing app must not be re-created")
	}
}

// TestRunnerFailsWhenLoginIncomplete proves that if the login does not actually
// authenticate (the operator did not complete the browser flow), the playbook stops with a
// clear message and registers no service rather than attempting to deploy.
func TestRunnerFailsWhenLoginIncomplete(t *testing.T) {
	ps, svc := stores(t)
	pb := flyPlaybook(t, ps)
	ex := &flyExecer{authed: false, loginNoOp: true}
	r := NewRunner(ex, fixedResolver{path: "/deps/flyctl"}, svc, WithConfirmer(autoConfirm{}))

	_, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"})
	if err == nil || !strings.Contains(err.Error(), "Fly login did not complete") {
		t.Fatalf("expected an incomplete-login error, got %v", err)
	}
	if ex.ran("deploy") {
		t.Fatal("must not deploy when login did not complete")
	}
	if _, err := svc.Get(context.Background(), "myapp"); err == nil {
		t.Fatal("a failed run must register no service")
	}
}

// TestRunnerLoginDeclinedStops proves that declining the login confirmation stops the run
// before the browser opens: no login, no deploy, no service.
func TestRunnerLoginDeclinedStops(t *testing.T) {
	ps, svc := stores(t)
	pb := flyPlaybook(t, ps)
	ex := &flyExecer{authed: false}
	r := NewRunner(ex, fixedResolver{path: "/deps/flyctl"}, svc, WithConfirmer(declineConfirm{}))

	if _, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"}); err == nil {
		t.Fatal("declining the login confirmation must stop the run")
	}
	if ex.ran("auth login") || ex.ran("deploy") {
		t.Fatal("a declined confirmation must not run login or deploy")
	}
	if _, err := svc.Get(context.Background(), "myapp"); err == nil {
		t.Fatal("a cancelled run must register no service")
	}
}

// TestRunnerFailsClosedWithoutConfirmer proves a non-interactive run (no confirmer wired)
// stops at the confirm step rather than silently logging in, so a confirm never proceeds
// without consent.
func TestRunnerFailsClosedWithoutConfirmer(t *testing.T) {
	ps, svc := stores(t)
	pb := flyPlaybook(t, ps)
	ex := &flyExecer{authed: false}
	r := NewRunner(ex, fixedResolver{path: "/deps/flyctl"}, svc) // no confirmer

	if _, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"}); err == nil {
		t.Fatal("a confirm step with no prompter must fail closed")
	}
	if ex.ran("auth login") {
		t.Fatal("must not log in when the confirm cannot be answered")
	}
}

// TestStoreRoundTrip proves a playbook is admitted, read back, and that one with no flow is
// refused.
func TestStoreRoundTrip(t *testing.T) {
	ps, _ := stores(t)
	ctx := context.Background()
	pb := putPlaybook(t, ps, "p", Spec{Flow: json.RawMessage(`{"steps":[{"op":"return","return":{"value":"ok"}}]}`)})
	if pb.Name != "p" {
		t.Fatalf("name: %q", pb.Name)
	}
	if _, err := ps.Put(ctx, "bad", Spec{}); err == nil {
		t.Fatal("a playbook with no flow must be refused")
	}
}
