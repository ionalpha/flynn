package driver_test

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
)

// fakeDriver is a no-op Driver for registry tests.
type fakeDriver struct{ name string }

func (f fakeDriver) Name() string { return f.name }
func (f fakeDriver) Build(driver.Spec) (goal.StepExecutor, goal.StopEvaluator, error) {
	return nil, nil, nil
}

func TestDefaultRegistryHasBuiltins(t *testing.T) {
	r := driver.Default()
	// The empty name resolves to the default loop.
	d, err := r.Resolve("")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if d.Name() != driver.NameDefault {
		t.Fatalf("empty name resolved to %q, want %q", d.Name(), driver.NameDefault)
	}
	if _, err := r.Resolve(driver.NameSingleShot); err != nil {
		t.Fatalf("single-shot should be registered: %v", err)
	}
	names := r.Names()
	if len(names) != 2 {
		t.Fatalf("expected 2 built-in drivers, got %v", names)
	}
}

func TestResolveUnknownFailsClosed(t *testing.T) {
	r := driver.Default()
	_, err := r.Resolve("does-not-exist")
	if err == nil {
		t.Fatal("an unknown driver must fail closed, not fall back to a default")
	}
	if got := fault.Classify(err); got != fault.Terminal {
		t.Fatalf("unknown driver error class = %s, want terminal", got)
	}
}

func TestRegisterRefusesNilEmptyAndDuplicate(t *testing.T) {
	r := driver.NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("registering nil must fail")
	}
	if err := r.Register(fakeDriver{name: ""}); err == nil {
		t.Fatal("registering an empty name must fail")
	}
	if err := r.Register(fakeDriver{name: "x"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register(fakeDriver{name: "x"}); err == nil {
		t.Fatal("registering a duplicate name must fail")
	}
}

func TestBuiltinDriversBuild(t *testing.T) {
	for _, name := range []string{driver.NameDefault, driver.NameSingleShot} {
		t.Run(name, func(t *testing.T) {
			d, err := driver.Default().Resolve(name)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			exec, stop, err := d.Build(driver.Spec{System: "be helpful"})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if exec == nil || stop == nil {
				t.Fatalf("build returned nil parts: exec=%v stop=%v", exec, stop)
			}
		})
	}
}

// TestRegistryResolveProperty is the rigor property: a registry resolves every
// name it registered to that exact driver, and any name it did not register fails
// closed. This holds for any set of distinct driver names.
func TestRegistryResolveProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		names := rapid.SliceOfNDistinct(rapid.StringMatching(`[a-z]{1,6}`), 0, 8,
			func(s string) string { return s }).Draw(rt, "names")

		r := driver.NewRegistry()
		for _, n := range names {
			if err := r.Register(fakeDriver{name: n}); err != nil {
				rt.Fatalf("register %q: %v", n, err)
			}
		}
		for _, n := range names {
			d, err := r.Resolve(n)
			if err != nil {
				rt.Fatalf("resolve %q: %v", n, err)
			}
			if d.Name() != n {
				rt.Fatalf("resolve %q returned %q", n, d.Name())
			}
		}
		// A name outside the set (uppercase prefix never matches the lowercase
		// generator) must fail closed.
		if _, err := r.Resolve("Z-not-registered"); err == nil {
			rt.Fatal("an unregistered name must fail closed")
		}
	})
}
