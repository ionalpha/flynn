package playbook

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/dependency"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/flow"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/secret"
)

// recordObserver collects the step events a run reports. The interpreter reports steps from
// the goroutine that called Run, so a plain slice is enough.
type recordObserver struct{ events []flow.StepEvent }

func (o *recordObserver) Step(e flow.StepEvent) { o.events = append(o.events, e) }

// verProber reports one version string for any program, standing in for running a real
// program's version command through the sandbox.
type verProber struct{ out string }

func (p verProber) Probe(context.Context, string, []string) (string, error) { return p.out, nil }

// depManager builds a dependency manager over a memory store holding one spec whose program
// is present on the host at the given version, so a resolve is satisfied without a download.
func depManager(t *testing.T, version string) *dependency.Manager {
	t.Helper()
	reg := resource.NewRegistry()
	if err := dependency.RegisterKind(reg); err != nil {
		t.Fatalf("register dependency kind: %v", err)
	}
	ds := dependency.NewStore(resource.NewMemory(reg))
	_, err := ds.Put(context.Background(), "flyctl", dependency.Spec{
		Binaries:    []string{"flyctl"},
		VersionArgs: []string{"version"},
		MinVersion:  "0.3.0",
	})
	if err != nil {
		t.Fatalf("put dependency: %v", err)
	}
	return dependency.NewManager(ds, fetch.New(), t.TempDir(), dependency.WithProber(verProber{out: version}))
}

// TestSandboxExecerRunsThroughTheSandbox proves the exec adapter runs the flow's command
// line in the sandbox and returns the exit code and output verbatim, so a playbook's exec
// step is confined rather than spawning a process directly.
func TestSandboxExecerRunsThroughTheSandbox(t *testing.T) {
	sb := &recordingSandbox{res: sandbox.ExecResult{ExitCode: 3, Output: "boom"}}
	ex := NewSandboxExecer(sb)

	res, err := ex.Exec(context.Background(), flow.ExecRequest{Command: "flyctl status"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if sb.line != "flyctl status" {
		t.Fatalf("the command did not reach the sandbox verbatim: %q", sb.line)
	}
	if res.ExitCode != 3 || res.Output != "boom" {
		t.Fatalf("the sandbox result was not passed through: %+v", res)
	}
}

// TestSandboxExecerFailsClosed proves the adapter fails closed with no sandbox wired, and
// surfaces a sandbox transport failure rather than reporting a zero exit code (which the
// flow would read as success).
func TestSandboxExecerFailsClosed(t *testing.T) {
	if _, err := NewSandboxExecer(nil).Exec(context.Background(), flow.ExecRequest{Command: "x"}); err == nil {
		t.Fatal("an exec with no sandbox must fail closed")
	}
	sb := &recordingSandbox{err: errors.New("sandbox unavailable")}
	res, err := NewSandboxExecer(sb).Exec(context.Background(), flow.ExecRequest{Command: "x"})
	if err == nil {
		t.Fatal("a sandbox transport failure must be returned")
	}
	if res.ExitCode != 0 || res.Output != "" {
		t.Fatalf("a failed exec must return no result: %+v", res)
	}
}

// TestManagerResolverYieldsThePath proves the dependency adapter satisfies a named
// dependency through the manager and hands the flow the path to run, and that an unknown
// dependency fails the step.
func TestManagerResolverYieldsThePath(t *testing.T) {
	r := NewManagerResolver(depManager(t, "flyctl v0.4.61"))

	path, err := r.Resolve(context.Background(), "flyctl")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if path != "flyctl" {
		t.Fatalf("expected the present program's path, got %q", path)
	}
	if _, err := r.Resolve(context.Background(), "not-a-dependency"); err == nil {
		t.Fatal("an unknown dependency must fail the step")
	}
}

// TestManagerResolverFailsClosed proves a runner wired with no dependency manager refuses a
// dependency step instead of returning an empty path the flow would then run as a command.
func TestManagerResolverFailsClosed(t *testing.T) {
	path, err := NewManagerResolver(nil).Resolve(context.Background(), "flyctl")
	if err == nil {
		t.Fatal("a dependency step with no manager must fail closed")
	}
	if path != "" {
		t.Fatalf("a failed resolve must yield no path, got %q", path)
	}
}

// provisionSpec is a playbook that resolves a dependency, runs it, materializes a secret
// into the provider, and returns a workload with no name (so the runner falls back to the
// playbook's own name).
func provisionSpec() Spec {
	return Spec{
		Service: &ServiceBlock{Provider: "fly", Target: service.TargetContainer},
		Flow: json.RawMessage(`{"steps":[
			{"id":"cli","op":"dependency","dependency":{"name":"flyctl"}},
			{"op":"exec","exec":{"command":"{{steps.cli.path}} version"}},
			{"op":"secret","secret":{"ref":"flynn/passphrase","sink":"fly","target":{
				"app":"{{config.app}}","key":"FLYNN_VAULT_PASSPHRASE","cli":"{{steps.cli.path}}"
			}}},
			{"op":"return","return":{"value":{
				"url":"https://{{config.app}}.fly.dev","address":{"app":"{{config.app}}"}
			}}}
		]}`),
	}
}

// TestRunnerObservesStepsAndStampsWithTheClock proves the observer port sees each effectful
// step begin and end, that the credential sink port materializes the playbook's secret, and
// that the registered service is stamped from the runner's clock rather than the wall
// clock. It also proves the fallback naming: a flow result with no name registers the
// service under the playbook's own name.
func TestRunnerObservesStepsAndStampsWithTheClock(t *testing.T) {
	ps, svc := stores(t)
	pb := putPlaybook(t, ps, "provision", provisionSpec())

	obs := &recordObserver{}
	sinkSB := &recordingSandbox{}
	sink := NewCredentialSink(fixedSource{val: secret.New("passphrase")}, sinkSB)
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	r := NewRunner(okExecer{}, fixedResolver{path: "/deps/flyctl"}, svc,
		WithObserver(obs), WithCredentialSink(sink), WithClock(clock.NewManual(at)))

	res, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Every effectful step is reported, beginning and ending, and never the pure return
	// step. The dependency step names the dependency; the exec step carries the rendered
	// command line, with the resolved path substituted in.
	begins := map[flow.Op]int{}
	ends := map[flow.Op]int{}
	sawExec := false
	for _, e := range obs.events {
		switch e.Phase {
		case flow.StepBegin:
			begins[e.Op]++
		case flow.StepEnd:
			ends[e.Op]++
		}
		if e.Op == flow.OpExec {
			sawExec = sawExec || strings.Contains(e.Detail, "/deps/flyctl version")
		}
		if e.Op == flow.OpReturn {
			t.Fatalf("a pure step must not be observed: %+v", e)
		}
	}
	if begins[flow.OpDependency] != 1 || ends[flow.OpDependency] != 1 {
		t.Fatalf("the dependency step was not observed beginning and ending: %+v", obs.events)
	}
	if begins[flow.OpExec] != 1 || ends[flow.OpExec] != 1 || !sawExec {
		t.Fatalf("the exec step was not observed with its rendered command: %+v", obs.events)
	}

	// The secret reached the provider through the sink, on standard input.
	if !strings.Contains(sinkSB.line, "secrets import") || !strings.Contains(sinkSB.line, "myapp") {
		t.Fatalf("the credential sink did not run the provider import: %q", sinkSB.line)
	}
	if got := string(sinkSB.stdin); got != "FLYNN_VAULT_PASSPHRASE=passphrase\n" {
		t.Fatalf("the secret was not delivered on stdin: %q", got)
	}

	// The flow returned no name, so the playbook's name is used, and the deploy stamp comes
	// from the runner's clock.
	if res.Service == nil || res.Service.Name != "provision" {
		t.Fatalf("expected the service to fall back to the playbook name: %+v", res.Service)
	}
	got, err := svc.Get(context.Background(), "provision")
	if err != nil {
		t.Fatalf("service not stored: %v", err)
	}
	if want := at.Format(time.RFC3339); got.Status.LastDeploy != want {
		t.Fatalf("the deploy stamp did not come from the runner's clock: %q want %q", got.Status.LastDeploy, want)
	}
}

// TestRunnerFailsClosedWithoutACredentialSink proves a playbook with a secret step refuses
// to run when no sink is wired, so a run can never quietly skip a credential the workload
// needs and report success.
func TestRunnerFailsClosedWithoutACredentialSink(t *testing.T) {
	ps, svc := stores(t)
	pb := putPlaybook(t, ps, "provision", provisionSpec())
	r := NewRunner(okExecer{}, fixedResolver{path: "/deps/flyctl"}, svc) // no sink

	if _, err := r.Run(context.Background(), pb, map[string]any{"app": "myapp"}); err == nil {
		t.Fatal("a secret step with no sink must fail closed")
	}
	if _, err := svc.Get(context.Background(), "provision"); err == nil {
		t.Fatal("a failed run must register no service")
	}
}

// TestRunnerWithoutAServiceStoreRunsButRegistersNothing proves a runner wired with no
// service store still runs a playbook that declares a service, and returns the flow's output
// with no service attached, rather than failing.
func TestRunnerWithoutAServiceStoreRunsButRegistersNothing(t *testing.T) {
	ps, _ := stores(t)
	pb := putPlaybook(t, ps, "demo", Spec{
		Service: &ServiceBlock{Provider: "fly"},
		Flow:    json.RawMessage(`{"steps":[{"op":"return","return":{"value":{"name":"n","url":"https://n"}}}]}`),
	})
	r := NewRunner(okExecer{}, fixedResolver{path: "/deps/flyctl"}, nil)

	res, err := r.Run(context.Background(), pb, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Service != nil {
		t.Fatalf("no service store means no registration, got %+v", res.Service)
	}
	if _, ok := res.Output.(map[string]any); !ok {
		t.Fatalf("the flow output must still be returned: %#v", res.Output)
	}
}

// TestRunnerRefusesAServicePlaybookThatReturnsNoObject proves a playbook that declares a
// service but whose flow returns a scalar fails the run, rather than registering a service
// with fabricated fields.
func TestRunnerRefusesAServicePlaybookThatReturnsNoObject(t *testing.T) {
	ps, svc := stores(t)
	pb := putPlaybook(t, ps, "scalar", Spec{
		Service: &ServiceBlock{Provider: "fly"},
		Flow:    json.RawMessage(`{"steps":[{"op":"return","return":{"value":"just-a-string"}}]}`),
	})
	r := NewRunner(okExecer{}, fixedResolver{path: "/deps/flyctl"}, svc)

	_, err := r.Run(context.Background(), pb, nil)
	if err == nil {
		t.Fatal("a service playbook whose flow returns no object must fail")
	}
	if !strings.Contains(err.Error(), "did not return an object") {
		t.Fatalf("the error should say what the flow failed to return: %v", err)
	}
	if _, err := svc.Get(context.Background(), "scalar"); err == nil {
		t.Fatal("a failed registration must leave no service behind")
	}
}

// TestRunnerDropsANonStringAddress proves a flow whose address object carries no string
// value registers the service with no addressing at all, rather than a malformed map the
// supervisor would later try to replay.
func TestRunnerDropsANonStringAddress(t *testing.T) {
	ps, svc := stores(t)
	pb := putPlaybook(t, ps, "numeric", Spec{
		Service: &ServiceBlock{Provider: "fly"},
		Flow: json.RawMessage(`{"steps":[{"op":"return","return":{"value":{
			"name":"numeric","url":"https://n","address":{"port":8080}
		}}}]}`),
	})
	r := NewRunner(okExecer{}, fixedResolver{path: "/deps/flyctl"}, svc)

	if _, err := r.Run(context.Background(), pb, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := svc.Get(context.Background(), "numeric")
	if err != nil {
		t.Fatalf("service not stored: %v", err)
	}
	if len(got.Spec.Address) != 0 {
		t.Fatalf("a non-string address must be dropped, got %v", got.Spec.Address)
	}
}

// TestRunnerSurfacesAServiceWriteFailure proves that when the flow succeeded but the
// service record cannot be written, the run fails loudly instead of reporting a workload
// Flynn is not actually holding in its desired state.
func TestRunnerSurfacesAServiceWriteFailure(t *testing.T) {
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register playbook kind: %v", err)
	}
	if err := service.RegisterKind(reg); err != nil {
		t.Fatalf("register service kind: %v", err)
	}
	mem := resource.NewMemory(reg)
	down := errors.New("backend down")
	svc := service.NewStore(&faultyStore{Store: mem, putErr: down})

	ps := NewStore(mem)
	pb := putPlaybook(t, ps, "demo", Spec{
		Service: &ServiceBlock{Provider: "fly"},
		Flow:    json.RawMessage(`{"steps":[{"op":"return","return":{"value":{"name":"n","url":"https://n"}}}]}`),
	})
	r := NewRunner(okExecer{}, fixedResolver{path: "/deps/flyctl"}, svc)

	res, err := r.Run(context.Background(), pb, nil)
	if err == nil {
		t.Fatal("a service write failure must fail the run")
	}
	if !errors.Is(err, down) {
		t.Fatalf("the run error must wrap the store failure: %v", err)
	}
	if res.Service != nil {
		t.Fatalf("a failed registration must return no service: %+v", res.Service)
	}
}

// TestRunnerRefusesAnInvalidFlow proves a stored playbook whose flow does not compile fails
// at the start of the run, before any port is touched.
func TestRunnerRefusesAnInvalidFlow(t *testing.T) {
	ps, svc := stores(t)
	pb := putPlaybook(t, ps, "broken", Spec{Flow: json.RawMessage(`{"steps":[{"op":"teleport","teleport":{}}]}`)})
	sb := &recordingSandbox{}
	r := NewRunner(NewSandboxExecer(sb), fixedResolver{path: "/deps/flyctl"}, svc)

	if _, err := r.Run(context.Background(), pb, nil); err == nil {
		t.Fatal("a playbook whose flow does not compile must not run")
	}
	if sb.line != "" {
		t.Fatalf("nothing must be executed for a flow that does not compile, ran %q", sb.line)
	}
}

// TestSinkFailsClosedWhenUnconfigured proves a sink with no secret source and no sandbox
// refuses to materialize anything, rather than reporting success without delivering the
// credential.
func TestSinkFailsClosedWhenUnconfigured(t *testing.T) {
	if err := NewCredentialSink(nil, nil).Put(context.Background(), "fly", "ref", map[string]string{
		"app": "a", "key": "K", "cli": "/deps/flyctl",
	}); err == nil {
		t.Fatal("an unconfigured sink must fail closed")
	}
}

// TestFlySinkRequiresAFullTarget proves the fly sink refuses an incomplete target rather
// than running a half-formed provider command, whichever field is missing.
func TestFlySinkRequiresAFullTarget(t *testing.T) {
	full := map[string]string{"app": "a", "key": "K", "cli": "/deps/flyctl"}
	for _, missing := range []string{"app", "key", "cli"} {
		t.Run("no-"+missing, func(t *testing.T) {
			target := map[string]string{}
			for k, v := range full {
				target[k] = v
			}
			delete(target, missing)

			sb := &recordingSandbox{}
			sink := NewCredentialSink(fixedSource{val: secret.New("x")}, sb)
			if err := sink.Put(context.Background(), "fly", "ref", target); err == nil {
				t.Fatalf("a target with no %s must be refused", missing)
			}
			if sb.line != "" {
				t.Fatalf("no provider command may run for an incomplete target, ran %q", sb.line)
			}
		})
	}
}
