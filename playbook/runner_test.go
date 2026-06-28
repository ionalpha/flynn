package playbook

import (
	"context"
	"encoding/json"
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

// scriptExecer returns a scripted result for the first command substring that matches, so a
// test can drive a playbook's control flow without running anything.
type scriptExecer struct {
	rules []execRule
	seen  []string
}

type execRule struct {
	match string
	res   flow.ExecResult
}

func (e *scriptExecer) Exec(_ context.Context, req flow.ExecRequest) (flow.ExecResult, error) {
	e.seen = append(e.seen, req.Command)
	for _, r := range e.rules {
		if strings.Contains(req.Command, r.match) {
			return r.res, nil
		}
	}
	return flow.ExecResult{ExitCode: 0}, nil
}

func (e *scriptExecer) ran(substr string) bool {
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

func putPlaybook(t *testing.T, s *Store, name string, spec Spec) Playbook {
	t.Helper()
	pb, err := s.Put(context.Background(), name, spec)
	if err != nil {
		t.Fatalf("put playbook: %v", err)
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
	r := NewRunner(&scriptExecer{}, fixedResolver{path: "/deps/flyctl"}, svc)

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

// TestRunnerDrivesFlyPlaybook runs the bundled Fly playbook end to end against scripted
// flyctl output: it ensures the CLI, sees the app is absent and creates it, deploys, and
// registers the service.
func TestRunnerDrivesFlyPlaybook(t *testing.T) {
	ps, svc := stores(t)
	if _, err := Sync(context.Background(), ps); err != nil {
		t.Fatalf("sync: %v", err)
	}
	pb, err := ps.Get(context.Background(), "fly-app")
	if err != nil {
		t.Fatalf("get fly-app: %v", err)
	}
	ex := &scriptExecer{rules: []execRule{
		{match: "auth whoami", res: flow.ExecResult{ExitCode: 0, Output: "ops@example.com"}},
		{match: "apps list", res: flow.ExecResult{ExitCode: 0, Output: "other-app\n"}}, // myapp absent
		{match: "apps create", res: flow.ExecResult{ExitCode: 0, Output: "created"}},
		{match: "deploy", res: flow.ExecResult{ExitCode: 0, Output: "deployed"}},
		{match: "status", res: flow.ExecResult{ExitCode: 0, Output: "running"}},
	}}
	r := NewRunner(ex, fixedResolver{path: "/deps/flyctl"}, svc)

	res, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !ex.ran("apps create myapp") {
		t.Fatal("expected the app to be created when absent")
	}
	if !ex.ran("deploy") {
		t.Fatal("expected a deploy")
	}
	if res.Service == nil || res.Service.Spec.URL != "https://myapp.fly.dev" {
		t.Fatalf("service not registered from the deploy: %+v", res.Service)
	}
}

// TestRunnerSkipsCreateWhenAppExists proves the playbook is level-triggered: when the app
// already exists, it is not created again.
func TestRunnerSkipsCreateWhenAppExists(t *testing.T) {
	ps, svc := stores(t)
	if _, err := Sync(context.Background(), ps); err != nil {
		t.Fatalf("sync: %v", err)
	}
	pb, _ := ps.Get(context.Background(), "fly-app")
	ex := &scriptExecer{rules: []execRule{
		{match: "auth whoami", res: flow.ExecResult{ExitCode: 0}},
		{match: "apps list", res: flow.ExecResult{ExitCode: 0, Output: "myapp\nother\n"}}, // present
		{match: "deploy", res: flow.ExecResult{ExitCode: 0}},
		{match: "status", res: flow.ExecResult{ExitCode: 0}},
	}}
	r := NewRunner(ex, fixedResolver{path: "/deps/flyctl"}, svc)
	if _, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if ex.ran("apps create") {
		t.Fatal("an existing app must not be re-created")
	}
}

// TestRunnerAuthHandoff proves the playbook stops with the login instruction when the
// operator is not authenticated, the human-handoff point, and registers no service.
func TestRunnerAuthHandoff(t *testing.T) {
	ps, svc := stores(t)
	if _, err := Sync(context.Background(), ps); err != nil {
		t.Fatalf("sync: %v", err)
	}
	pb, _ := ps.Get(context.Background(), "fly-app")
	ex := &scriptExecer{rules: []execRule{
		{match: "auth whoami", res: flow.ExecResult{ExitCode: 1, Output: "not logged in"}},
	}}
	r := NewRunner(ex, fixedResolver{path: "/deps/flyctl"}, svc)

	_, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"})
	if err == nil || !strings.Contains(err.Error(), "fly auth login") {
		t.Fatalf("expected the auth handoff message, got %v", err)
	}
	if ex.ran("deploy") {
		t.Fatal("must not deploy when unauthenticated")
	}
	if _, err := svc.Get(context.Background(), "myapp"); err == nil {
		t.Fatal("a failed run must register no service")
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
