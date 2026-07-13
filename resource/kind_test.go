package resource_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

// rejectingCompiler is an injected SchemaCompiler: it counts the schemas it
// compiles and returns a Validator that rejects every instance, so a test can
// prove admission ran through the injected engine rather than the built-in one.
type rejectingCompiler struct {
	compiled atomic.Int64
}

func (c *rejectingCompiler) Compile([]byte) (resource.Validator, error) {
	c.compiled.Add(1)
	return validatorFunc(func(any) error { return errors.New("rejected by the injected compiler") }), nil
}

type validatorFunc func(any) error

func (f validatorFunc) Validate(instance any) error { return f(instance) }

// failingCompiler refuses to compile at all, so a kind carrying any schema is
// rejected at registration.
type failingCompiler struct{}

func (failingCompiler) Compile([]byte) (resource.Validator, error) {
	return nil, errors.New("compiler is unavailable")
}

// TestRegisterRejectsIncompleteKind gates registration admission: a kind without
// both an APIVersion and a Name has no address, so it must never enter the
// registry.
func TestRegisterRejectsIncompleteKind(t *testing.T) {
	cases := []struct {
		name string
		kind resource.Kind
	}{
		{"no apiVersion", resource.Kind{Name: "Widget"}},
		{"no name", resource.Kind{APIVersion: "test/v1"}},
		{"neither", resource.Kind{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := resource.NewRegistry().Register(tc.kind)
			if !errors.Is(err, resource.ErrInvalid) {
				t.Fatalf("Register(%+v) err = %v, want ErrInvalid", tc.kind, err)
			}
		})
	}
}

// TestRegisterRejectsUncompilableSchema keeps a broken schema out of the registry
// at registration time rather than failing at the first write, so a kind that is
// registered is always usable.
func TestRegisterRejectsUncompilableSchema(t *testing.T) {
	reg := resource.NewRegistry()
	err := reg.Register(resource.Kind{
		APIVersion: "test/v1",
		Name:       "Widget",
		Schema:     json.RawMessage(`{"type":"string","pattern":"[unclosed"}`),
	})
	if !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("Register with a bad schema err = %v, want ErrInvalid", err)
	}
	if _, ok := reg.Lookup("test/v1", "Widget"); ok {
		t.Fatal("a kind whose schema failed to compile must not be registered")
	}
}

// TestLookupAndKindsIntrospectTheRegistry gates the introspection surface ("what
// can this agent represent?"): Lookup answers by address, Kinds returns every
// registered kind in a deterministic group-then-name order.
func TestLookupAndKindsIntrospectTheRegistry(t *testing.T) {
	reg := resource.NewRegistry()
	want := []resource.Kind{
		{APIVersion: "a.test/v1", Name: "Beta"},
		{APIVersion: "a.test/v1", Name: "Alpha"},
		{APIVersion: "b.test/v1", Name: "Alpha"},
	}
	for _, k := range want {
		if err := reg.Register(k); err != nil {
			t.Fatalf("register %v: %v", k, err)
		}
	}

	got, ok := reg.Lookup("a.test/v1", "Alpha")
	if !ok {
		t.Fatal("Lookup missed a registered kind")
	}
	if got.APIVersion != "a.test/v1" || got.Name != "Alpha" {
		t.Fatalf("Lookup returned %+v, want a.test/v1 Alpha", got)
	}
	if _, ok := reg.Lookup("a.test/v1", "Absent"); ok {
		t.Fatal("Lookup reported an unregistered kind as present")
	}
	if _, ok := reg.Lookup("absent/v1", "Alpha"); ok {
		t.Fatal("Lookup ignored the API group")
	}

	kinds := reg.Kinds()
	var order []string
	for _, k := range kinds {
		order = append(order, k.APIVersion+"/"+k.Name)
	}
	wantOrder := []string{"a.test/v1/Alpha", "a.test/v1/Beta", "b.test/v1/Alpha"}
	if fmt.Sprint(order) != fmt.Sprint(wantOrder) {
		t.Fatalf("Kinds order = %v, want %v", order, wantOrder)
	}
}

// TestRegisterReplacesAnExistingKind locks the documented upsert behaviour: a
// re-registration under the same address replaces the kind (its new schema is the
// one admission enforces), so a kind authored at runtime can be revised.
func TestRegisterReplacesAnExistingKind(t *testing.T) {
	reg := resource.NewRegistry()
	if err := reg.Register(resource.Kind{APIVersion: "test/v1", Name: "Widget"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Validate("test/v1", "Widget", []byte(`{"size":"m"}`)); err != nil {
		t.Fatalf("an unconstrained kind must admit any spec: %v", err)
	}
	if err := reg.Register(resource.Kind{
		APIVersion: "test/v1", Name: "Widget",
		Schema:   json.RawMessage(`{"type":"object","required":["size"]}`),
		Singular: "widget", Plural: "widgets",
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Validate("test/v1", "Widget", []byte(`{}`)); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("the replacement schema must be enforced, err = %v", err)
	}
	if k, _ := reg.Lookup("test/v1", "Widget"); k.Plural != "widgets" {
		t.Fatalf("Lookup returned the stale kind: %+v", k)
	}
	if n := len(reg.Kinds()); n != 1 {
		t.Fatalf("a replacement must not add a second kind, got %d", n)
	}
}

// TestValidateRejectsUnregisteredKindAndBadSpec covers the two admission refusals
// a store depends on: an unregistered kind is never storable, and a spec that is
// not valid JSON is refused before it reaches a schema.
func TestValidateRejectsUnregisteredKindAndBadSpec(t *testing.T) {
	reg := resource.NewRegistry()
	if err := reg.Register(resource.Kind{
		APIVersion: "test/v1", Name: "Widget",
		Schema: json.RawMessage(`{"type":"object"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Validate("test/v1", "Absent", []byte(`{}`)); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("unregistered kind err = %v, want ErrInvalid", err)
	}
	if err := reg.Validate("test/v1", "Widget", []byte(`{"size":`)); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("a spec that is not valid JSON must be ErrInvalid, got %v", err)
	}
	// An empty spec is an empty object, so a schema with no required fields admits it.
	if err := reg.Validate("test/v1", "Widget", nil); err != nil {
		t.Fatalf("an empty spec must be admitted by an unconstrained object schema: %v", err)
	}
}

// TestWithSchemaCompilerReplacesAdmissionEngine proves the compiler is a port: an
// injected engine is the one that compiles registered schemas and the one whose
// verdict admission returns, so a host can swap in a full JSON Schema engine
// without the core depending on it.
func TestWithSchemaCompilerReplacesAdmissionEngine(t *testing.T) {
	c := &rejectingCompiler{}
	reg := resource.NewRegistry(resource.WithSchemaCompiler(c))
	schema := json.RawMessage(`{"type":"object"}`)
	if err := reg.Register(resource.Kind{APIVersion: "test/v1", Name: "Widget", Schema: schema}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := c.compiled.Load(); got != 1 {
		t.Fatalf("the injected compiler compiled %d schemas, want 1", got)
	}
	// The built-in engine would admit this spec; the injected one rejects it.
	if err := reg.Validate("test/v1", "Widget", []byte(`{}`)); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("admission must return the injected validator's verdict, got %v", err)
	}

	// A nil compiler leaves the built-in in place rather than disabling admission.
	def := resource.NewRegistry(resource.WithSchemaCompiler(nil))
	if err := def.Register(resource.Kind{
		APIVersion: "test/v1", Name: "Widget",
		Schema: json.RawMessage(`{"type":"object","required":["size"]}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := def.Validate("test/v1", "Widget", []byte(`{}`)); !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("a nil compiler must keep the built-in engine, got %v", err)
	}
}

// TestInjectedCompilerFailureRejectsRegistration proves a compile failure from the
// injected engine is surfaced at registration, not swallowed into an
// unconstrained kind.
func TestInjectedCompilerFailureRejectsRegistration(t *testing.T) {
	reg := resource.NewRegistry(resource.WithSchemaCompiler(failingCompiler{}))
	err := reg.Register(resource.Kind{
		APIVersion: "test/v1", Name: "Widget",
		Schema: json.RawMessage(`{"type":"object"}`),
	})
	if !errors.Is(err, resource.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	// A kind with no schema never reaches the compiler, so it still registers.
	if err := reg.Register(resource.Kind{APIVersion: "test/v1", Name: "Free"}); err != nil {
		t.Fatalf("a schemaless kind must not need the compiler: %v", err)
	}
}
