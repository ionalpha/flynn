package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// haltAction is the reference verb the action-gate tests exercise. The actual
// halt/pause/run verbs are owned by the kill-switch and lifecycle layers; this stands
// in for one, recording whether its body ran so a test can prove a denied action has
// no side effect.
const haltActionName = "instance.halt"

// actionServer builds a server registering the reference "halt" verb (operator scope,
// action instance.halt) with the given local grant, and a token table covering an
// operator with full authority, an operator narrowed to exclude halt, and a read-only
// caller. ran is set true only if the verb body executes.
func actionServer(t *testing.T, local capability.Grant, ran *bool) (http.Handler, spine.Log) {
	t.Helper()
	store, log := newStore(t)
	putWidget(t, store, "w1")
	auth := NewTokenAuthenticator(map[string]Principal{
		// A broad operator: full action authority, bounded only by the instance.
		"optok": {ID: "op", Scope: ScopeOperator, Grant: capability.AllowAll()},
		// An operator whose grant was narrowed across the wire to exclude halt.
		"narrowtok": {ID: "narrow", Scope: ScopeOperator, Grant: capability.NewGrant("instance.pause")},
		// A read-only caller: must not reach an operator verb at all.
		"readtok": {ID: "r", Scope: ScopeRead, Grant: capability.AllowAll()},
	})
	srv := NewServer(store, log, auth, WithLocalGrant(local), WithAction("halt", ActionSpec{
		Action:   haltActionName,
		MinScope: ScopeOperator,
		Run: func(context.Context, Principal, resource.Resource) (any, error) {
			*ran = true
			return "halted", nil
		},
	}))
	return srv.Handler(), log
}

func doPost(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r, _ := http.NewRequest(http.MethodPost, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func lastAudit(t *testing.T, log spine.Log) spine.Event {
	t.Helper()
	evs, err := log.Read(context.Background(), spine.Query{Stream: AuditStream})
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("no audit events recorded")
	}
	return evs[len(evs)-1]
}

// TestActionAdmittedWhenBothGrantsAllow: a full-authority operator, against an instance
// whose local grant admits the verb, runs it and is audited allowed.
func TestActionAdmittedWhenBothGrantsAllow(t *testing.T) {
	var ran bool
	h, log := actionServer(t, capability.AllowAll(), &ran)

	rec := doPost(t, h, "/v1/Widget/w1/halt", "optok")
	if rec.Code != http.StatusOK {
		t.Fatalf("admitted action code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !ran {
		t.Fatal("the verb body must run once admitted")
	}
	assertAudit(t, lastAudit(t, log), "op", decisionAllowed)
}

// TestActionRefusesReadToken: an operator verb refuses a read-scoped token before the
// grant gate is even consulted, with no side effect, audited forbidden (scope).
func TestActionRefusesReadToken(t *testing.T) {
	var ran bool
	h, log := actionServer(t, capability.AllowAll(), &ran)

	rec := doPost(t, h, "/v1/Widget/w1/halt", "readtok")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read token on operator verb code = %d, want 403", rec.Code)
	}
	if ran {
		t.Fatal("a scope-forbidden action must not run")
	}
	assertAudit(t, lastAudit(t, log), "r", decisionForbidden)
}

// TestActionDeniedWhenRemoteGrantExcludesIt is the core no-escalation property: the
// instance LOCALLY allows the verb (AllowAll), but the caller's grant was narrowed
// across the wire to exclude it, so the intersection denies it even though scope passes
// and the target would do it locally. No side effect; audited as a grant denial.
func TestActionDeniedWhenRemoteGrantExcludesIt(t *testing.T) {
	var ran bool
	h, log := actionServer(t, capability.AllowAll(), &ran)

	rec := doPost(t, h, "/v1/Widget/w1/halt", "narrowtok")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("narrowed-grant action code = %d, want 403", rec.Code)
	}
	if ran {
		t.Fatal("an action outside the narrowed grant must not run")
	}
	ev := lastAudit(t, log)
	assertAudit(t, ev, "narrow", decisionDenied)
	if v, _ := ev.Payload["action"].(string); v != "halt" {
		t.Fatalf("audit action = %q, want the verb halt", v)
	}
}

// TestActionDeniedWhenLocalGrantExcludesIt is the other half of the intersection: even
// a full-authority caller (AllowAll) cannot exceed the instance's own local ceiling, so
// a local grant that omits the verb denies it. This is the instance bounding a token,
// independent of how broad the token is.
func TestActionDeniedWhenLocalGrantExcludesIt(t *testing.T) {
	var ran bool
	// Local grant admits only pause, not halt.
	h, log := actionServer(t, capability.NewGrant("instance.pause"), &ran)

	rec := doPost(t, h, "/v1/Widget/w1/halt", "optok")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("action beyond the local grant code = %d, want 403", rec.Code)
	}
	if ran {
		t.Fatal("an action beyond the instance's local grant must not run")
	}
	assertAudit(t, lastAudit(t, log), "op", decisionDenied)
}

// TestActionRefusesUnauthenticated: no token is a 401, audited, with no side effect.
func TestActionRefusesUnauthenticated(t *testing.T) {
	var ran bool
	h, log := actionServer(t, capability.AllowAll(), &ran)

	rec := doPost(t, h, "/v1/Widget/w1/halt", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated action code = %d, want 401", rec.Code)
	}
	if ran {
		t.Fatal("an unauthenticated action must not run")
	}
	assertAudit(t, lastAudit(t, log), "", decisionUnauthenticated)
}

// TestActionMissingResourceIs404: a verb on a nonexistent resource is a 404 and is not
// audited as an access decision, because nothing was acted on.
func TestActionMissingResourceIs404(t *testing.T) {
	var ran bool
	h, log := actionServer(t, capability.AllowAll(), &ran)

	rec := doPost(t, h, "/v1/Widget/ghost/halt", "optok")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing-resource action code = %d, want 404", rec.Code)
	}
	if ran {
		t.Fatal("a verb on a missing resource must not run")
	}
	if evs, _ := log.Read(context.Background(), spine.Query{Stream: AuditStream}); len(evs) != 0 {
		t.Fatalf("a 404 must not record an access decision, got %d events", len(evs))
	}
}
