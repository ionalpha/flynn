package profilestore

import (
	"testing"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/resource"
)

// newStore builds an in-memory resource store with the ModelProfile kind registered, the setup
// every test runs against.
func newStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	return resource.NewMemory(reg)
}

// TestWriteThenRead proves the loop: a measured profile written to the store is read back through
// the Source as the same capability fingerprint the harness consumes.
func TestWriteThenRead(t *testing.T) {
	rs := newStore(t)
	spec := Spec{
		ModelID:              "ollama:qwen2",
		Quant:                "Q4_K_M",
		Runtime:              "llama.cpp",
		BatteryVersion:       "1",
		ToolCallReliability:  0.8,
		StructuredOutput:     0.6,
		InstructionFollowing: 0.5,
		EffectiveContext:     8192,
	}
	if err := Write(t.Context(), rs, spec); err != nil {
		t.Fatal(err)
	}

	src, err := NewSource(t.Context(), rs)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := src.Profile("ollama:qwen2")
	if !ok {
		t.Fatal("profile not found after write")
	}
	want := harness.ModelProfile{ToolCallReliability: 0.8, StructuredOutput: 0.6, InstructionFollowing: 0.5, EffectiveContext: 8192}
	if got != want {
		t.Fatalf("profile = %+v, want %+v", got, want)
	}
}

// TestWriteUpsertsByTarget proves a re-measurement of the same model+quant+runtime overwrites
// rather than accumulating, so the store holds one current profile per target.
func TestWriteUpsertsByTarget(t *testing.T) {
	rs := newStore(t)
	base := Spec{ModelID: "m", Quant: "Q4_K_M", Runtime: "llama.cpp", ToolCallReliability: 0.5}
	if err := Write(t.Context(), rs, base); err != nil {
		t.Fatal(err)
	}
	base.ToolCallReliability = 0.9
	if err := Write(t.Context(), rs, base); err != nil {
		t.Fatal(err)
	}
	all, err := rs.ListAll(t.Context(), Kind, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("re-measurement should upsert, got %d resources", len(all))
	}
	src, _ := NewSource(t.Context(), rs)
	if p, _ := src.Profile("m"); p.ToolCallReliability != 0.9 {
		t.Fatalf("latest write should win, got %v", p.ToolCallReliability)
	}
}

// TestMultipleQuantsFoldConservative proves a model measured at several quantizations is reported
// as no better than its weakest build, so the harness never under-scaffolds on the strength of a
// build that may not be the one served.
func TestMultipleQuantsFoldConservative(t *testing.T) {
	rs := newStore(t)
	if err := Write(t.Context(), rs, Spec{ModelID: "m", Quant: "Q8_0", ToolCallReliability: 0.9, StructuredOutput: 0.9, InstructionFollowing: 0.9, EffectiveContext: 16384}); err != nil {
		t.Fatal(err)
	}
	if err := Write(t.Context(), rs, Spec{ModelID: "m", Quant: "Q3_K_S", ToolCallReliability: 0.4, StructuredOutput: 0.7, InstructionFollowing: 0.3, EffectiveContext: 4096}); err != nil {
		t.Fatal(err)
	}
	src, _ := NewSource(t.Context(), rs)
	got, _ := src.Profile("m")
	want := harness.ModelProfile{ToolCallReliability: 0.4, StructuredOutput: 0.7, InstructionFollowing: 0.3, EffectiveContext: 4096}
	if got != want {
		t.Fatalf("folded profile = %+v, want the element-wise minimum %+v", got, want)
	}
}

// TestUnknownModelIsAbsent proves a model with no stored profile reads as unknown, the input that
// makes the harness scaffold most conservatively.
func TestUnknownModelIsAbsent(t *testing.T) {
	src, err := NewSource(t.Context(), newStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.Profile("never-measured"); ok {
		t.Fatal("an unmeasured model must be absent")
	}
}

// TestSchemaRejectsOutOfRange proves the stored schema guards the data: a score outside [0,1] is
// refused at the store boundary, so a corrupt profile cannot drive scaffolding.
func TestSchemaRejectsOutOfRange(t *testing.T) {
	rs := newStore(t)
	err := Write(t.Context(), rs, Spec{ModelID: "m", ToolCallReliability: 1.5})
	if err == nil {
		t.Fatal("a reliability score above 1 must be rejected by the schema")
	}
}

// TestSourceFeedsHarnessPlan proves the end-to-end intent: a stored profile, read through the
// Source and mapped by the harness, yields the lean plan a reliable model deserves rather than the
// conservative default.
func TestSourceFeedsHarnessPlan(t *testing.T) {
	rs := newStore(t)
	if err := Write(t.Context(), rs, Spec{ModelID: "m", ToolCallReliability: 1, StructuredOutput: 1, InstructionFollowing: 1}); err != nil {
		t.Fatal(err)
	}
	src, _ := NewSource(t.Context(), rs)
	plan := harness.PlanFor(src, "m", 8192)
	if plan.ConstrainToolCalls || plan.SimplifyToolSchemas || plan.VerifyPasses != 0 {
		t.Fatalf("a fully-reliable stored profile must yield the lean plan, got %+v", plan)
	}
	// An unmeasured model through the same source stays fully scaffolded.
	if got := harness.PlanFor(src, "other", 8192); !got.ConstrainToolCalls {
		t.Fatalf("an unmeasured model must stay conservative, got %+v", got)
	}
}
