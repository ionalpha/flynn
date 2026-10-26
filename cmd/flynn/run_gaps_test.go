package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/skill/skilltool"
)

// refusesKind is a resource store that rejects writes of one kind, so a test can fail
// exactly the step it targets while the rest of the run assembles normally.
type refusesKind struct {
	resource.Store
	kind string
	err  error
}

func (s refusesKind) Put(ctx context.Context, r resource.Resource) (resource.Resource, error) {
	if r.Kind == s.kind {
		return resource.Resource{}, s.err
	}
	return s.Store.Put(ctx, r)
}

// codexAgent is an external agent whose driver assembles into a run without launching
// anything.
func codexAgent(t *testing.T) *externAgent {
	t.Helper()
	ea := &externAgent{
		model:  "gpt-5-codex",
		driver: externagent.NewDriver(externagent.NewCodex("", nil), nil, t.TempDir()),
	}
	t.Cleanup(ea.close)
	return ea
}

// budgetedRequest is a driveRequest for a run under a spend ceiling whose budget pool
// cannot be written.
func budgetedRequest(t *testing.T, boom error) driveRequest {
	t.Helper()
	store := memStore(t)
	reg := mustRegistry(t)
	cfg := driveConfig{}
	withBudget(budgetpkg.Limits{Tokens: 1000, Cost: 1})(&cfg)
	return driveRequest{
		out:       &bytes.Buffer{},
		model:     llmtest.NewScripted(llmtest.SayText("unreachable")),
		plan:      harness.Plan{},
		workdir:   t.TempDir(),
		system:    defaultSystemPrompt,
		objective: "do the thing",
		rstore:    refusesKind{Store: store.Resources(reg), kind: budgetpkg.Kind, err: boom},
		jq:        store.Jobs(),
		log:       store.Log(),
		cfg:       cfg,
	}
}

// TestExecuteRefusesToRunWhenTheBudgetCannotBeOpened checks that a run asked to stay
// under a ceiling does not start without one: an unopened pool reads as unlimited, so
// proceeding would spend with the cap the operator set silently absent.
func TestExecuteRefusesToRunWhenTheBudgetCannotBeOpened(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	boom := errors.New("pool store is down")
	req := budgetedRequest(t, boom)

	_, _, _, err := req.execute(ctx)

	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "open run budget") {
		t.Fatalf("err = %v, want the open-budget failure wrapping the store's error", err)
	}
}

// TestAssembleBuildsAnExternalAgentRun checks that a request carrying an external agent
// is assembled around that agent's episode driver and not as a native conversation, and
// that an assembly failure is reported rather than swallowed by execute.
func TestAssembleBuildsAnExternalAgentRun(t *testing.T) {
	store := memStore(t)
	reg := mustRegistry(t)
	ea := codexAgent(t)
	req := driveRequest{
		workdir: t.TempDir(),
		system:  defaultSystemPrompt,
		rstore:  store.Resources(reg),
		jq:      store.Jobs(),
		log:     store.Log(),
		cfg:     driveConfig{extAgent: ea, skills: skilltool.New(store.Skills()), shell: checkShell},
	}

	run, err := req.assemble()
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })

	if run.sess == nil || run.rt == nil {
		t.Fatal("the external run was assembled without a session or a runtime")
	}
	if run.parts.grant.Allows("spawn") {
		t.Fatal("an external run was assembled as a fan-out run")
	}
}

// TestDeclareProvenanceOnADrivenRun checks the two things a driven external run says
// when its declaration is incomplete: the record could not take it, and the harness
// reported events the record could not hold. Both are reported to the operator because
// the sealed record alone would read as natively driven or silently short.
func TestDeclareProvenanceOnADrivenRun(t *testing.T) {
	tests := []struct {
		name       string
		closeStore bool
		unrecorded int
		want       string
	}{
		{"harness events the record could not hold", false, 2, "2 attested event(s) not recorded"},
		{"declaration the record could not take", true, 0, "provenance not recorded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := memStore(t)
			reg := mustRegistry(t)
			req := driveRequest{
				workdir: t.TempDir(),
				system:  defaultSystemPrompt,
				rstore:  store.Resources(reg),
				jq:      store.Jobs(),
				log:     store.Log(),
				cfg:     driveConfig{extAgent: codexAgent(t), skills: skilltool.New(store.Skills())},
			}
			run, err := req.assemble()
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}
			t.Cleanup(func() { _ = run.Close() })
			// The run is assembled around a real driver; the declaration is read from a
			// stand-in that reports exactly the account under test.
			req.cfg.extAgent = &externAgent{driver: &tallyingDriver{stubHarnessDriver{
				name:       "codex",
				unrecorded: tc.unrecorded,
				unrecErr:   errors.New("the record was closed mid-episode"),
			}}}
			if tc.closeStore {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer

			req.declareProvenance(context.Background(), &out, run)

			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("output %q does not say %q", out.String(), tc.want)
			}
		})
	}
}
