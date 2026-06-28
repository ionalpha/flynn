package controlplane

import (
	"context"
	"net/http"
	"testing"

	"github.com/ionalpha/flynn/spine"
)

func TestAccessDecisionsAreAuditedToSpine(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")
	auth := NewTokenAuthenticator(map[string]Principal{
		"readtok": {ID: "alice", Scope: ScopeRead},
		"nonetok": {ID: "bob", Scope: ScopeNone},
	})
	h := NewServer(store, log, auth).Handler()

	if rec := do(t, h, "/v1/Widget", "readtok"); rec.Code != http.StatusOK {
		t.Fatalf("allowed read code = %d", rec.Code)
	}
	if rec := do(t, h, "/v1/Widget", "nonetok"); rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden code = %d", rec.Code)
	}
	if rec := do(t, h, "/v1/Widget", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated code = %d", rec.Code)
	}

	evs, err := log.Read(context.Background(), spine.Query{Stream: AuditStream})
	if err != nil {
		t.Fatalf("read audit stream: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("want 3 audit events, got %d", len(evs))
	}
	// Each outcome is recorded in order, attributed to the principal, with the failures
	// captured too: an allowed read by alice, a forbidden attempt by bob, and an
	// unauthenticated attempt with no principal.
	assertAudit(t, evs[0], "alice", decisionAllowed)
	assertAudit(t, evs[1], "bob", decisionForbidden)
	assertAudit(t, evs[2], "", decisionUnauthenticated)

	for _, e := range evs {
		if e.Stream != AuditStream || e.Type != EvAccess {
			t.Fatalf("audit event on wrong stream/type: %+v", e)
		}
		if m, _ := e.Payload["method"].(string); m != http.MethodGet {
			t.Fatalf("audit method = %v, want GET", e.Payload["method"])
		}
		if p, _ := e.Payload["path"].(string); p != "/v1/Widget" {
			t.Fatalf("audit path = %v", e.Payload["path"])
		}
		if req, _ := e.Payload["required"].(string); req != ScopeRead.String() {
			t.Fatalf("audit required scope = %v, want read", e.Payload["required"])
		}
	}
}

func assertAudit(t *testing.T, e spine.Event, wantPrincipal, wantDecision string) {
	t.Helper()
	if e.Principal != wantPrincipal {
		t.Fatalf("audit principal = %q, want %q", e.Principal, wantPrincipal)
	}
	if d, _ := e.Payload["decision"].(string); d != wantDecision {
		t.Fatalf("audit decision = %q, want %q", d, wantDecision)
	}
}
