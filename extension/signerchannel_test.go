package extension

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/secret"
)

// signerStub serves the two tools a signer extension advertises, over the real MCP transport,
// so these tests exercise the actual channel rather than a stand-in for it.
type signerStub struct {
	key       ed25519.PrivateKey
	unlockOut string // overrides the unlock reply when set
	signOut   string // overrides the sign reply when set
	unlockErr error
	signErr   error

	gotPassphrase string
	gotPayload    []byte
}

func (s *signerStub) tools(t *testing.T) []mission.Tool {
	t.Helper()
	return []mission.Tool{
		stubTool{
			name: SignerUnlockTool,
			invoke: func(_ context.Context, input json.RawMessage) (string, error) {
				var args struct {
					Passphrase string `json:"passphrase"`
				}
				_ = json.Unmarshal(input, &args)
				s.gotPassphrase = args.Passphrase
				if s.unlockErr != nil {
					return "", s.unlockErr
				}
				if s.unlockOut != "" {
					return s.unlockOut, nil
				}
				pub, _ := s.key.Public().(ed25519.PublicKey)
				return `{"publicKey":"` + base64.StdEncoding.EncodeToString(pub) + `","curve":"ed25519"}`, nil
			},
		},
		stubTool{
			name: SignerSignTool,
			invoke: func(_ context.Context, input json.RawMessage) (string, error) {
				var args struct {
					Payload string `json:"payload"`
				}
				_ = json.Unmarshal(input, &args)
				s.gotPayload, _ = base64.StdEncoding.DecodeString(args.Payload)
				if s.signErr != nil {
					return "", s.signErr
				}
				if s.signOut != "" {
					return s.signOut, nil
				}
				sig := ed25519.Sign(s.key, s.gotPayload)
				return `{"signature":"` + base64.StdEncoding.EncodeToString(sig) + `"}`, nil
			},
		},
	}
}

// channelTo mounts a signer stub and returns the host's private channel to it.
func channelTo(t *testing.T, s *signerStub) SignerChannel {
	t.Helper()
	h, _, m := mountStub(t, s.tools(t))
	ch, err := h.SignerChannelFor(m.ID)
	if err != nil {
		t.Fatalf("SignerChannelFor: %v", err)
	}
	return ch
}

func signerKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	return ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
}

// TestChannelUnlockAndSign drives the real channel end to end over MCP: the passphrase reaches
// the signer, the public key comes back, and a payload is signed with the key the signer holds.
func TestChannelUnlockAndSign(t *testing.T) {
	ctx := context.Background()
	key := signerKey(t)
	stub := &signerStub{key: key}
	ch := channelTo(t, stub)

	pub, curve, err := ch.Unlock(ctx, secret.New("open sesame"))
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if stub.gotPassphrase != "open sesame" {
		t.Fatalf("the signer was unlocked with %q, not the operator's passphrase", stub.gotPassphrase)
	}
	if !ed25519.PublicKey(pub).Equal(key.Public().(ed25519.PublicKey)) {
		t.Fatal("the channel returned a key that is not the signer's")
	}
	if curve != "ed25519" {
		t.Fatalf("curve = %q, want ed25519", curve)
	}

	payload := []byte("a transaction the host never reads")
	sig, err := ch.SignPayload(ctx, payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if string(stub.gotPayload) != string(payload) {
		t.Fatalf("the signer was asked to sign %q, not the payload it was given", stub.gotPayload)
	}
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), payload, sig) {
		t.Fatal("the signature the channel returned does not verify")
	}
}

// TestChannelUnlockRefusesAnEmptyPassphrase: the host does not even ask. An empty passphrase
// means the vault held nothing, and that is an operator mistake worth naming rather than a
// sealed key that mysteriously will not open.
func TestChannelUnlockRefusesAnEmptyPassphrase(t *testing.T) {
	stub := &signerStub{key: signerKey(t)}
	ch := channelTo(t, stub)

	if _, _, err := ch.Unlock(context.Background(), secret.Text{}); err == nil {
		t.Fatal("the host tried to unlock a signer with no passphrase")
	}
	if stub.gotPassphrase != "" {
		t.Fatal("the host sent an empty passphrase to the signer instead of refusing outright")
	}
}

// TestChannelUnlockRejectsABadReply: the signer's answer is untrusted output from another
// process. A reply that is not JSON, or that carries no usable key, must fail the unlock rather
// than yield an empty key that later signs nothing anybody can verify.
func TestChannelUnlockRejectsABadReply(t *testing.T) {
	ctx := context.Background()

	for name, reply := range map[string]string{
		"not json":   `i am not json`,
		"no key":     `{"publicKey":"","curve":"ed25519"}`,
		"not base64": `{"publicKey":"@@@@","curve":"ed25519"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ch := channelTo(t, &signerStub{key: signerKey(t), unlockOut: reply})
			if _, _, err := ch.Unlock(ctx, secret.New("p")); err == nil {
				t.Fatal("a signer that answered with no usable key was unlocked anyway")
			}
		})
	}
}

// TestChannelUnlockCarriesTheSignersRefusal: a wrong passphrase is the signer's to report, and
// its reason has to reach the operator, who is the only one who can act on it.
func TestChannelUnlockCarriesTheSignersRefusal(t *testing.T) {
	stub := &signerStub{key: signerKey(t), unlockErr: errors.New("the sealed key did not open")}
	ch := channelTo(t, stub)

	_, _, err := ch.Unlock(context.Background(), secret.New("wrong"))
	if err == nil {
		t.Fatal("the signer refused to unlock but the host carried on")
	}
	if !strings.Contains(err.Error(), "did not open") {
		t.Fatalf("the signer's reason did not reach the caller: %v", err)
	}
}

// TestChannelSignCarriesTheRefusalAndYieldsNoSignature is the property the whole design rests
// on. When the signer refuses a transaction, no signature comes back, and the rule it refused
// on reaches the operator.
func TestChannelSignCarriesTheRefusalAndYieldsNoSignature(t *testing.T) {
	stub := &signerStub{
		key:     signerKey(t),
		signErr: errors.New("the message mints tokens but does not revoke the mint authority"),
	}
	ch := channelTo(t, stub)

	sig, err := ch.SignPayload(context.Background(), []byte("a draining transaction"))
	if err == nil {
		t.Fatal("the signer refused but the host produced a signature anyway")
	}
	if len(sig) != 0 {
		t.Fatal("a refusal returned signature bytes")
	}
	if !strings.Contains(err.Error(), "does not revoke the mint authority") {
		t.Fatalf("the rule the signer refused on did not reach the caller: %v", err)
	}
}

// TestChannelSignRejectsAnEmptySignature: a signer that approves but hands back nothing must not
// be read as a successful signature over nothing. "I approved" with no signature is a failure.
func TestChannelSignRejectsAnEmptySignature(t *testing.T) {
	ctx := context.Background()

	for name, reply := range map[string]string{
		"empty signature":  `{"signature":""}`,
		"absent signature": `{}`,
		"not base64":       `{"signature":"@@@@"}`,
		"not json":         `i approve`,
	} {
		t.Run(name, func(t *testing.T) {
			ch := channelTo(t, &signerStub{key: signerKey(t), signOut: reply})
			if _, err := ch.SignPayload(ctx, []byte("x")); err == nil {
				t.Fatal("a signer that returned no usable signature was treated as a success")
			}
		})
	}
}

// TestChannelReportsADeadSigner: a signer whose process has gone must surface as a failure, not
// as a signature nobody produced. A mint that cannot reach its signer has to stop.
func TestChannelReportsADeadSigner(t *testing.T) {
	stub := &signerStub{key: signerKey(t)}
	h, conn, m := mountStub(t, stub.tools(t))
	ch, err := h.SignerChannelFor(m.ID)
	if err != nil {
		t.Fatalf("SignerChannelFor: %v", err)
	}

	// The signer dies underneath us.
	if err := conn.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if _, err := ch.SignPayload(context.Background(), []byte("x")); err == nil {
		t.Fatal("signing against a dead signer reported success")
	}
	if _, _, err := ch.Unlock(context.Background(), secret.New("p")); err == nil {
		t.Fatal("unlocking a dead signer reported success")
	}
}

// TestSignerChannelForRefusesAnUnmountedExtension: there is no channel to a signer that is not
// running, and asking for one fails rather than returning something that cannot sign.
func TestSignerChannelForRefusesAnUnmountedExtension(t *testing.T) {
	h, _, _ := mountStub(t, []mission.Tool{stubTool{name: "token_verify", invoke: neverCalled(t)}})

	if _, err := h.SignerChannelFor("no-such-extension"); err == nil {
		t.Fatal("got a channel to a signer that is not mounted")
	}
}

// TestReservedSignerToolNames pins which names the host keeps for itself. They are the two the
// channel calls, and nothing else: a name added here without a channel to match would be a tool
// silently unreachable to everybody.
func TestReservedSignerToolNames(t *testing.T) {
	for _, name := range []string{SignerUnlockTool, SignerSignTool} {
		if !reservedSignerTool(name) {
			t.Fatalf("%q must be reserved for the host", name)
		}
	}
	if reservedSignerTool("token_mint") {
		t.Fatal("an ordinary worker tool was treated as a host-only signer tool")
	}
}
