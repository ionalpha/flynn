package credential

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	return NewStore(resource.NewMemory(reg))
}

func TestRolePermits(t *testing.T) {
	cases := []struct {
		cred, required Role
		want           bool
	}{
		{RoleAdmin, RoleRead, true},
		{RoleAdmin, RoleAdmin, true},
		{RoleOperator, RoleRead, true},
		{RoleOperator, RoleAdmin, false},
		{RoleRead, RoleOperator, false},
		{RoleRead, RoleRead, true},
		{"", RoleAdmin, false}, // a role-less credential cannot meet a role requirement
		{"", RoleRead, false},
		{RoleRead, "", true}, // an action requiring no role admits any credential
		{"", "", true},
	}
	for _, c := range cases {
		if got := c.cred.Permits(c.required); got != c.want {
			t.Fatalf("Role(%q).Permits(%q) = %v, want %v", c.cred, c.required, got, c.want)
		}
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleRead, RoleOperator, RoleAdmin} {
		if !r.Valid() {
			t.Fatalf("%q should be valid", r)
		}
	}
	for _, r := range []Role{"", "root", "superuser"} {
		if r.Valid() {
			t.Fatalf("%q should not be valid", r)
		}
	}
}

func TestPutAndGet(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, err := s.Put(ctx, Spec{Integration: "cloudflare", Name: "prod", AuthType: "bearer", Role: RoleOperator})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(ctx, "cloudflare", "prod")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.AuthType != "bearer" || got.Spec.Role != RoleOperator {
		t.Fatalf("spec: %+v", got.Spec)
	}
	if got.Ref() != "cloudflare/prod" {
		t.Fatalf("ref: %q", got.Ref())
	}
}

func TestPutNeverStoresSecret(t *testing.T) {
	// The Spec has no field for a secret value, so a marshalled credential resource
	// cannot carry one. This guards the invariant that secrets stay in the vault.
	body, err := json.Marshal(Spec{Integration: "x", Name: "y", VaultRef: "x/y"})
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"value", "secret", "token", "password"} {
		if _, ok := generic[banned]; ok {
			t.Fatalf("credential spec must not carry a secret field %q", banned)
		}
	}
}

func TestPutValidation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, Spec{Name: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for missing integration, got %v", err)
	}
	if _, err := s.Put(ctx, Spec{Integration: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for missing name, got %v", err)
	}
	if _, err := s.Put(ctx, Spec{Integration: "x", Name: "y", Role: "root"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for bad role, got %v", err)
	}
}

func TestGetNotFound(t *testing.T) {
	s := newStore(t)
	if _, err := s.Get(context.Background(), "x", "y"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
