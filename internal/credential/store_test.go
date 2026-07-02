package credential

import (
	"context"
	"errors"
	"testing"
)

func TestDefaultSelection(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustPut(t, s, Spec{Integration: "cloudflare", Name: "prod", IsDefault: true})
	mustPut(t, s, Spec{Integration: "cloudflare", Name: "staging"})

	def, err := s.Default(ctx, "cloudflare")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if def.Spec.Name != "prod" {
		t.Fatalf("default is %q", def.Spec.Name)
	}
}

func TestSingleDefaultInvariant(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustPut(t, s, Spec{Integration: "cf", Name: "a", IsDefault: true})
	mustPut(t, s, Spec{Integration: "cf", Name: "b", IsDefault: true}) // should displace a

	creds, err := s.List(ctx, "cf")
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	var which string
	for _, c := range creds {
		if c.Spec.IsDefault {
			defaults++
			which = c.Spec.Name
		}
	}
	if defaults != 1 || which != "b" {
		t.Fatalf("expected exactly one default 'b', got %d (%q)", defaults, which)
	}
}

func TestSetDefault(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustPut(t, s, Spec{Integration: "cf", Name: "a", IsDefault: true})
	mustPut(t, s, Spec{Integration: "cf", Name: "b"})

	if err := s.SetDefault(ctx, "cf", "b"); err != nil {
		t.Fatalf("set default: %v", err)
	}
	def, _ := s.Default(ctx, "cf")
	if def.Spec.Name != "b" {
		t.Fatalf("default is %q, want b", def.Spec.Name)
	}
	a, _ := s.Get(ctx, "cf", "a")
	if a.Spec.IsDefault {
		t.Fatal("a should no longer be default")
	}
}

func TestResolve(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustPut(t, s, Spec{Integration: "cf", Name: "prod", IsDefault: true})
	mustPut(t, s, Spec{Integration: "cf", Name: "staging"})

	// By name.
	byName, err := s.Resolve(ctx, "cf/staging")
	if err != nil || byName.Spec.Name != "staging" {
		t.Fatalf("resolve by name: %v %+v", err, byName)
	}
	// Bare integration resolves to the default.
	byDefault, err := s.Resolve(ctx, "cf")
	if err != nil || byDefault.Spec.Name != "prod" {
		t.Fatalf("resolve default: %v %+v", err, byDefault)
	}
}

func TestResolveBareIntegrationNoDefault(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustPut(t, s, Spec{Integration: "cf", Name: "only"}) // not marked default
	if _, err := s.Resolve(ctx, "cf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a bare integration with no default should be ErrNotFound, got %v", err)
	}
}

func TestListIsScopedToIntegration(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustPut(t, s, Spec{Integration: "cf", Name: "a"})
	mustPut(t, s, Spec{Integration: "vercel", Name: "a"})
	mustPut(t, s, Spec{Integration: "cf", Name: "b"})

	cf, err := s.List(ctx, "cf")
	if err != nil {
		t.Fatal(err)
	}
	if len(cf) != 2 || cf[0].Spec.Name != "a" || cf[1].Spec.Name != "b" {
		t.Fatalf("cf credentials: %+v", cf)
	}
}

func TestDelete(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustPut(t, s, Spec{Integration: "cf", Name: "gone"})
	if err := s.Delete(ctx, "cf", "gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, "cf", "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestSlashRejected(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, Spec{Integration: "a/b", Name: "c"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a slash in integration should be rejected, got %v", err)
	}
	if _, err := s.Put(ctx, Spec{Integration: "a", Name: "b/c"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a slash in name should be rejected, got %v", err)
	}
}

func TestResolveMalformed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, ref := range []string{"", "/", "cf/"} {
		if _, err := s.Resolve(ctx, ref); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ref %q should be ErrInvalid, got %v", ref, err)
		}
	}
}

func mustPut(t *testing.T, s *Store, spec Spec) {
	t.Helper()
	if _, err := s.Put(context.Background(), spec); err != nil {
		t.Fatalf("put %s/%s: %v", spec.Integration, spec.Name, err)
	}
}
