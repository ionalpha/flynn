package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

const (
	approvalHost   = "host-1"
	wipeAction     = "instance.wipe"
	wipeTargetW1   = "Widget/w1"
	approverKeyID  = "approver-a"
	approverKeyID2 = "approver-b"
)

// apNow is a fixed instant so an approval window is deterministic.
var apNow = time.Unix(1_900_000_000, 0).UTC()

// dangerServer builds a server exposing a dangerous "wipe" verb (admin scope, action
// instance.wipe, quorum default 1 unless overridden) gated by an approval verifier over
// the given keyring. ran is set when the verb body runs. The returned signer's key is
// authorized in the verifier; a second authorized signer is returned for M-of-N tests.
func dangerServer(t *testing.T, quorum int, ran *bool) (http.Handler, spine.Log, *approval.Ed25519Signer, *approval.Ed25519Signer) {
	t.Helper()
	store, log := newStore(t)
	putWidget(t, store, "w1")

	keyring := approval.NewKeyring()
	sigA, pubA, err := approval.GenerateEd25519Signer(approverKeyID, bytes.NewReader(bytes.Repeat([]byte{0x11}, 64)))
	if err != nil {
		t.Fatalf("gen signer A: %v", err)
	}
	sigB, pubB, err := approval.GenerateEd25519Signer(approverKeyID2, bytes.NewReader(bytes.Repeat([]byte{0x22}, 64)))
	if err != nil {
		t.Fatalf("gen signer B: %v", err)
	}
	if err := keyring.Add(approverKeyID, pubA); err != nil {
		t.Fatalf("add key A: %v", err)
	}
	if err := keyring.Add(approverKeyID2, pubB); err != nil {
		t.Fatalf("add key B: %v", err)
	}
	verifier := approval.NewVerifier(keyring, approval.NewMemStore(),
		approval.WithHost(approvalHost), approval.WithClock(clock.NewManual(apNow)))

	auth := NewTokenAuthenticator(map[string]Principal{
		// An admin caller with full authority: it still cannot run a dangerous verb
		// without a valid approval, which is the point of the gate.
		"admintok": {ID: "admin", Scope: ScopeAdmin, Grant: capability.AllowAll()},
	})
	srv := NewServer(store, log, auth,
		WithApprovals(verifier, approvalHost),
		WithAction("wipe", ActionSpec{
			Action:    wipeAction,
			MinScope:  ScopeOperator,
			Dangerous: true,
			Quorum:    quorum,
			Run: func(context.Context, Principal, resource.Resource) (any, error) {
				*ran = true
				return "wiped", nil
			},
		}))
	return srv.Handler(), log, sigA, sigB
}

// mintApproval signs env and encodes it as a single X-Flynn-Approval header value.
func mintApproval(t *testing.T, signer approval.Signer, env approval.Envelope) string {
	t.Helper()
	ap, err := signer.Sign(env)
	if err != nil {
		t.Fatalf("sign approval: %v", err)
	}
	raw, err := json.Marshal(ap)
	if err != nil {
		t.Fatalf("marshal approval: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// wipeEnvelope is the binding the server requires for wipe on w1 by the admin caller:
// action, principal, target detail, and host, with a caller-supplied nonce and window.
func wipeEnvelope(nonce string, expiry time.Time) approval.Envelope {
	return approval.Envelope{
		Action:    wipeAction,
		Principal: "admin",
		Detail:    wipeTargetW1,
		Host:      approvalHost,
		Nonce:     nonce,
		Expiry:    expiry.UnixNano(),
	}
}

// doPostApprovals POSTs the verb with the given approval header values attached.
func doPostApprovals(t *testing.T, h http.Handler, path, token string, approvals ...string) *httptest.ResponseRecorder {
	t.Helper()
	r, _ := http.NewRequest(http.MethodPost, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for _, a := range approvals {
		r.Header.Add(approvalHeader, a)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func auditEvents(t *testing.T, log spine.Log) []spine.Event {
	t.Helper()
	evs, err := log.Read(context.Background(), spine.Query{Stream: AuditStream})
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	return evs
}

func hasApprovalRecord(evs []spine.Event, granted bool) bool {
	for _, e := range evs {
		if e.Type != EvApproval {
			continue
		}
		if g, _ := e.Payload["granted"].(bool); g == granted {
			return true
		}
	}
	return false
}

// TestDangerousActionAdmittedWithValidApproval: a valid, bound, in-window approval from
// an authorized key admits the dangerous verb; it runs and both the access decision and
// the approval are recorded.
func TestDangerousActionAdmittedWithValidApproval(t *testing.T) {
	var ran bool
	h, log, sigA, _ := dangerServer(t, 1, &ran)
	hdr := mintApproval(t, sigA, wipeEnvelope("n1", apNow.Add(time.Hour)))

	rec := doPostApprovals(t, h, "/v1/Widget/w1/wipe", "admintok", hdr)
	if rec.Code != http.StatusOK {
		t.Fatalf("approved dangerous action code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !ran {
		t.Fatal("an approved dangerous verb must run")
	}
	evs := auditEvents(t, log)
	assertAudit(t, evs[len(evs)-1], "admin", decisionAllowed)
	if !hasApprovalRecord(evs, true) {
		t.Fatal("a granted approval must be recorded on the spine")
	}
}

// TestDangerousActionRefusedWithoutApproval is the core property: an admin caller with
// full grant is still refused a dangerous verb when it presents no approval, and the
// refused attempt is recorded. Scope and grant are not enough.
func TestDangerousActionRefusedWithoutApproval(t *testing.T) {
	var ran bool
	h, log, _, _ := dangerServer(t, 1, &ran)

	rec := doPostApprovals(t, h, "/v1/Widget/w1/wipe", "admintok")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unapproved dangerous action code = %d, want 403", rec.Code)
	}
	if ran {
		t.Fatal("a dangerous verb must not run without approval, even for an admin")
	}
	evs := auditEvents(t, log)
	assertAudit(t, evs[len(evs)-1], "admin", decisionDenied)
	if !hasApprovalRecord(evs, false) {
		t.Fatal("a denied approval must be recorded on the spine")
	}
}

// TestDangerousActionRefusesExpiredApproval: an approval past its expiry does not count.
func TestDangerousActionRefusesExpiredApproval(t *testing.T) {
	var ran bool
	h, _, sigA, _ := dangerServer(t, 1, &ran)
	// Expiry already passed relative to the verifier's clock (apNow).
	hdr := mintApproval(t, sigA, wipeEnvelope("n1", apNow.Add(-time.Second)))

	rec := doPostApprovals(t, h, "/v1/Widget/w1/wipe", "admintok", hdr)
	if rec.Code != http.StatusForbidden || ran {
		t.Fatalf("expired approval must be refused: code=%d ran=%v", rec.Code, ran)
	}
}

// TestDangerousActionRefusesMismatchedApproval: an approval bound to a different target
// does not authorize this one, even though it is otherwise valid.
func TestDangerousActionRefusesMismatchedApproval(t *testing.T) {
	var ran bool
	h, _, sigA, _ := dangerServer(t, 1, &ran)
	wrong := wipeEnvelope("n1", apNow.Add(time.Hour))
	wrong.Detail = "Widget/other" // signed for a different resource
	hdr := mintApproval(t, sigA, wrong)

	rec := doPostApprovals(t, h, "/v1/Widget/w1/wipe", "admintok", hdr)
	if rec.Code != http.StatusForbidden || ran {
		t.Fatalf("mismatched-target approval must be refused: code=%d ran=%v", rec.Code, ran)
	}
}

// TestDangerousActionRefusesForgedApproval: a signature from a key not in the verifier's
// keyring is not authorized, so a self-signed approval cannot mint authority.
func TestDangerousActionRefusesForgedApproval(t *testing.T) {
	var ran bool
	h, _, _, _ := dangerServer(t, 1, &ran)
	// A signer whose key was never added to the keyring.
	forger, _, err := approval.GenerateEd25519Signer("forger", bytes.NewReader(bytes.Repeat([]byte{0x33}, 64)))
	if err != nil {
		t.Fatalf("gen forger: %v", err)
	}
	hdr := mintApproval(t, forger, wipeEnvelope("n1", apNow.Add(time.Hour)))

	rec := doPostApprovals(t, h, "/v1/Widget/w1/wipe", "admintok", hdr)
	if rec.Code != http.StatusForbidden || ran {
		t.Fatalf("forged-key approval must be refused: code=%d ran=%v", rec.Code, ran)
	}
}

// TestDangerousApprovalIsSingleUse: the same approval cannot be replayed; its nonce is
// burned on the first successful use.
func TestDangerousApprovalIsSingleUse(t *testing.T) {
	var ran bool
	h, _, sigA, _ := dangerServer(t, 1, &ran)
	hdr := mintApproval(t, sigA, wipeEnvelope("n1", apNow.Add(time.Hour)))

	if rec := doPostApprovals(t, h, "/v1/Widget/w1/wipe", "admintok", hdr); rec.Code != http.StatusOK {
		t.Fatalf("first use code = %d, want 200", rec.Code)
	}
	ran = false
	if rec := doPostApprovals(t, h, "/v1/Widget/w1/wipe", "admintok", hdr); rec.Code != http.StatusForbidden {
		t.Fatalf("replayed approval code = %d, want 403", rec.Code)
	}
	if ran {
		t.Fatal("a replayed approval must not run the verb again")
	}
}

// TestDangerousActionFailsClosedWithoutVerifier: a verb declared dangerous on a server
// with no approval verifier configured cannot run at all (misconfiguration fails closed).
func TestDangerousActionFailsClosedWithoutVerifier(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")
	auth := NewTokenAuthenticator(map[string]Principal{
		"admintok": {ID: "admin", Scope: ScopeAdmin, Grant: capability.AllowAll()},
	})
	var ran bool
	// Register a dangerous verb but deliberately omit WithApprovals.
	srv := NewServer(store, log, auth, WithAction("wipe", ActionSpec{
		Action:    wipeAction,
		MinScope:  ScopeOperator,
		Dangerous: true,
		Run: func(context.Context, Principal, resource.Resource) (any, error) {
			ran = true
			return nil, nil
		},
	}))
	rec := doPostApprovals(t, srv.Handler(), "/v1/Widget/w1/wipe", "admintok")
	if rec.Code != http.StatusForbidden || ran {
		t.Fatalf("dangerous verb without a verifier must fail closed: code=%d ran=%v", rec.Code, ran)
	}
}

// TestDangerousActionQuorum: a 2-of-N verb refuses a single signature and admits only
// when two distinct authorized approvers have signed the same binding.
func TestDangerousActionQuorum(t *testing.T) {
	var ran bool
	h, _, sigA, sigB := dangerServer(t, 2, &ran)
	// Each approver signs the same binding with their own single-use nonce, as distinct
	// approvers independently would.
	one := mintApproval(t, sigA, wipeEnvelope("n-a", apNow.Add(time.Hour)))

	if rec := doPostApprovals(t, h, "/v1/Widget/w1/wipe", "admintok", one); rec.Code != http.StatusForbidden {
		t.Fatalf("one of two signatures code = %d, want 403", rec.Code)
	}
	if ran {
		t.Fatal("a sub-quorum must not run the verb")
	}
	two := mintApproval(t, sigB, wipeEnvelope("n-b", apNow.Add(time.Hour)))
	if rec := doPostApprovals(t, h, "/v1/Widget/w1/wipe", "admintok", one, two); rec.Code != http.StatusOK {
		t.Fatalf("two distinct signatures code = %d, want 200", rec.Code)
	}
	if !ran {
		t.Fatal("a met quorum must run the verb")
	}
}
