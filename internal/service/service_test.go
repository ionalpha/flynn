package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	return NewStore(resource.NewMemory(reg))
}

// TestServiceRoundTrip proves a deployed workload is recorded with its spec and
// status, listed, and retired.
func TestServiceRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	spec := Spec{
		Provider:     "cloudflare",
		Target:       TargetStaticSite,
		ExternalID:   "dep-1",
		URL:          "https://site.example",
		DesiredState: StateRunning,
		Credential:   "cloudflare/prod",
	}
	status := Status{Phase: "deployed", ObservedURL: "https://site.example"}
	if _, err := s.Put(ctx, "my-site", spec, status); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := s.Get(ctx, "my-site")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Provider != "cloudflare" || got.Spec.URL != "https://site.example" {
		t.Fatalf("spec round-trip: %+v", got.Spec)
	}
	if got.Status.Phase != "deployed" {
		t.Fatalf("status round-trip: %+v", got.Status)
	}

	list, err := s.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %d", err, len(list))
	}

	if err := s.Delete(ctx, "my-site"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, "my-site"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	// Deleting an absent service is not an error (teardown is idempotent).
	if err := s.Delete(ctx, "my-site"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
}

// TestServiceRequiresProvider proves a service with no provider is refused.
func TestServiceRequiresProvider(t *testing.T) {
	s := testStore(t)
	if _, err := s.Put(context.Background(), "x", Spec{}, Status{}); err == nil {
		t.Fatal("expected a service with no provider to be rejected")
	}
}
