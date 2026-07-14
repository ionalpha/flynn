package extension

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
)

// signStub is a stub extension tool that speaks the host-signing handshake: on its first call
// it opens a session and asks for a signature; on each continuation it records the signature
// it was given and asks for the next one, until it has collected `rounds` of them and returns
// a terminal result. It lets a test prove the host drives the loop, injects the key, and signs
// the exact bytes.
type signStub struct {
	rounds int

	mu         sync.Mutex
	firstInput json.RawMessage
	payloads   [][]byte // the bytes it asked the host to sign, in order
	sigs       [][]byte // the signatures it received, in order
	signErrs   []string // any signing-failure messages it received
	seq        int
	done       map[string]int
}

func newSignStub(rounds int) *signStub { return &signStub{rounds: rounds, done: map[string]int{}} }

func (s *signStub) Def() llm.Tool {
	return llm.Tool{Name: "sign", Description: "host-signed op", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (s *signStub) Invoke(_ context.Context, in json.RawMessage) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var cont struct {
		Session   string `json:"session"`
		Signature string `json:"signature"`
		SignError string `json:"signError"`
	}
	_ = json.Unmarshal(in, &cont)

	if cont.Session == "" {
		s.firstInput = append(json.RawMessage(nil), in...)
		s.seq++
		id := fmt.Sprintf("s%d", s.seq)
		return s.emit(id)
	}
	if cont.SignError != "" {
		s.signErrs = append(s.signErrs, cont.SignError)
		return `{"done":true,"error":"` + cont.SignError + `"}`, nil
	}
	sig, err := base64.StdEncoding.DecodeString(cont.Signature)
	if err != nil {
		return "", err
	}
	s.sigs = append(s.sigs, sig)
	s.done[cont.Session]++
	return s.emit(cont.Session)
}

// emit returns the next signing request for a session, or the terminal result once the session
// has collected all its signatures.
func (s *signStub) emit(id string) (string, error) {
	if s.done[id] >= s.rounds {
		return `{"done":true,"result":"ok"}`, nil
	}
	payload := []byte(fmt.Sprintf("payload-%s-%d", id, s.done[id]))
	s.payloads = append(s.payloads, payload)
	return `{"session":"` + id + `","sign":{"message":"` + base64.StdEncoding.EncodeToString(payload) + `"}}`, nil
}

// testSigner is a deterministic ed25519 host signer (fixed seed, no randomness) for tests.
func testSigner(t *testing.T) *Ed25519HostSigner {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	s, err := NewEd25519HostSigner(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s
}

// TestHostSigningDrivesHandshake proves the host runs the full signing loop for a signing-
// enabled tool: it injects the granted key on the first call, signs each payload the tool
// hands out with that key, feeds the signature back, and returns the terminal result. Each
// signature verifies against the exact payload, so the host signed the right bytes.
func TestHostSigningDrivesHandshake(t *testing.T) {
	signer := testSigner(t)
	stub := newSignStub(3)
	h, _, m := mountStub(t, []mission.Tool{stub},
		WithHostSigner(func(ext, tool string) HostSigner {
			if ext == "token" && tool == "sign" {
				return signer
			}
			return nil
		}),
		// These tests drive the handshake with opaque test payloads, not real transactions,
		// so they name the policy that approves anything. Naming it is the point: a grant
		// with no policy signs nothing, so blind signing cannot happen by omission.
		WithSignPolicy(func(string, string) SignPolicy { return AnyPayload{} }))

	out, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{"foo":"bar"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out, `"done":true`) || !strings.Contains(out, `"ok"`) {
		t.Fatalf("did not get the terminal result: %q", out)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()

	// The granted key's public bytes were injected on the first call, and the caller's own
	// argument was preserved.
	var first map[string]json.RawMessage
	if err := json.Unmarshal(stub.firstInput, &first); err != nil {
		t.Fatalf("first input not an object: %v", err)
	}
	var hostKey string
	if err := json.Unmarshal(first["_hostKey"], &hostKey); err != nil {
		t.Fatalf("no _hostKey injected: %v", err)
	}
	if hostKey != base64.StdEncoding.EncodeToString(signer.Public()) {
		t.Fatalf("_hostKey is not the granted key")
	}
	if string(first["foo"]) != `"bar"` {
		t.Fatalf("caller argument was not preserved: %s", stub.firstInput)
	}

	// Three payloads signed, three signatures delivered, each verifying against its payload.
	if len(stub.sigs) != 3 || len(stub.payloads) != 3 {
		t.Fatalf("want 3 payloads and 3 signatures, got %d payloads / %d sigs", len(stub.payloads), len(stub.sigs))
	}
	for i, sig := range stub.sigs {
		if !ed25519.Verify(signer.Public(), stub.payloads[i], sig) {
			t.Fatalf("signature %d does not verify against its payload", i)
		}
	}
}

// TestHostSigningDeliversSignFailure proves a signer error is delivered to the tool as a
// signing-failure message (so the tool runs its own failure path), not swallowed or hung.
func TestHostSigningDeliversSignFailure(t *testing.T) {
	stub := newSignStub(3)
	failing := failingSigner{pub: testSigner(t).Public()}
	h, _, m := mountStub(t, []mission.Tool{stub},
		WithHostSigner(func(string, string) HostSigner { return failing }),
		WithSignPolicy(func(string, string) SignPolicy { return AnyPayload{} }))

	out, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.signErrs) != 1 || !strings.Contains(stub.signErrs[0], "vault unavailable") {
		t.Fatalf("signing failure was not delivered to the tool: %v", stub.signErrs)
	}
	if !strings.Contains(out, `"done":true`) {
		t.Fatalf("tool did not reach a terminal result after the signing failure: %q", out)
	}
}

// TestHostSigningBudgetEnforced proves a tool that never terminates the handshake is stopped
// at the signature limit rather than spinning the host forever.
func TestHostSigningBudgetEnforced(t *testing.T) {
	stub := newSignStub(1000) // never terminates within the budget
	h, _, m := mountStub(t, []mission.Tool{stub},
		WithHostSigner(func(string, string) HostSigner { return testSigner(t) }),
		WithSignPolicy(func(string, string) SignPolicy { return AnyPayload{} }),
		WithMaxSignatures(3))

	if _, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected the signature budget to stop an unbounded signing loop")
	}
}

// TestNonSigningToolUnaffected proves a tool with no granted signer is a plain single-call
// forward: no key is injected and the result passes through unchanged.
func TestNonSigningToolUnaffected(t *testing.T) {
	stub := stubTool{
		name: "sign", desc: "d",
		invoke: func(_ context.Context, in json.RawMessage) (string, error) { return "echo:" + string(in), nil },
	}
	h, _, m := mountStub(t, []mission.Tool{stub}) // no WithHostSigner
	out, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if strings.Contains(out, "_hostKey") {
		t.Fatalf("a non-signing tool must not have a key injected: %q", out)
	}
	if out != `echo:{"a":1}` {
		t.Fatalf("unexpected passthrough: %q", out)
	}
}

type failingSigner struct{ pub []byte }

func (f failingSigner) Public() []byte { return f.pub }
func (f failingSigner) Sign(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("vault unavailable")
}
