package extension

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
)

// stubSignerTool stands in for one tool of a mounted signer extension: it records what it was
// asked and returns whatever the test wants, including a refusal.
type stubSignerTool struct {
	name  string
	reply string
	err   error
	calls []json.RawMessage
}

func (s *stubSignerTool) Def() llm.Tool { return llm.Tool{Name: s.name} }

func (s *stubSignerTool) Invoke(_ context.Context, input json.RawMessage) (string, error) {
	s.calls = append(s.calls, input)
	if s.err != nil {
		return "", s.err
	}
	return s.reply, nil
}

// signerPair builds the two tools a signer extension exposes, backed by a real key so
// signatures actually verify.
func signerPair(t *testing.T, key ed25519.PrivateKey) (*stubSignerTool, *stubSignerTool) {
	t.Helper()
	pub := key.Public().(ed25519.PublicKey)
	public := &stubSignerTool{
		name:  SignerPublicTool,
		reply: `{"publicKey":"` + base64.StdEncoding.EncodeToString(pub) + `","curve":"ed25519"}`,
	}
	sign := &stubSignerTool{name: SignerSignTool}
	return public, sign
}

// TestExtensionSignerRoutesToTheSigner proves the whole point of the route: the host gets a
// verifying signature over the exact bytes, from a key it never held.
func TestExtensionSignerRoutesToTheSigner(t *testing.T) {
	ctx := context.Background()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	public, sign := signerPair(t, key)

	payload := []byte("a transaction the host cannot read")
	sign.reply = `{"signature":"` + base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload)) + `"}`

	signer, err := NewRoutedSigner(ctx, public, sign)
	if err != nil {
		t.Fatalf("NewRoutedSigner: %v", err)
	}
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), payload, mustSign(ctx, t, signer, payload)) {
		t.Fatal("the signature the signer extension returned does not verify")
	}

	// The host handed over exactly the bytes it was given, and nothing else.
	var asked struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(sign.calls[0], &asked); err != nil {
		t.Fatalf("the host sent the signer something that is not a signing request: %v", err)
	}
	got, err := base64.StdEncoding.DecodeString(asked.Payload)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("the host sent the signer %q, not the payload it was asked to sign", got)
	}
}

// TestRoutedSignerPublishesTheSignersKey: the host reports the signer's public key as its own
// signing identity, so a worker builds its transaction against the key that will actually sign
// it. A host that advertised a different key would have every transaction rejected by the
// chain, or worse, signed by something nobody expected.
func TestRoutedSignerPublishesTheSignersKey(t *testing.T) {
	ctx := context.Background()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	public, sign := signerPair(t, key)

	signer, err := NewRoutedSigner(ctx, public, sign)
	if err != nil {
		t.Fatalf("NewRoutedSigner: %v", err)
	}
	if !ed25519.PublicKey(signer.Public()).Equal(key.Public().(ed25519.PublicKey)) {
		t.Fatal("the host advertised a key that is not the signer's")
	}
}

// TestRoutedSignerRejectsAMalformedPublicReply: the signer's answer is untrusted output from
// another process. A reply that is not JSON must fail the mount, not be read as an empty key.
func TestRoutedSignerRejectsAMalformedPublicReply(t *testing.T) {
	public := &stubSignerTool{name: SignerPublicTool, reply: `i am not json`}
	if _, err := NewRoutedSigner(context.Background(), public, &stubSignerTool{name: SignerSignTool}); err == nil {
		t.Fatal("a signer whose public-key reply is not JSON was mounted anyway")
	}
}

// TestRoutedSignerDrivesAMountedTool is the whole route, end to end: a worker tool asks the
// host to sign, the host routes to the signer, and the worker is resumed with a signature it
// can use. No policy is configured in the host, and none is needed: the signer polices itself,
// which is the entire reason the host no longer has to understand the payload.
func TestRoutedSignerDrivesAMountedTool(t *testing.T) {
	ctx := context.Background()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	public, sign := signerPair(t, key)
	payload := []byte("an unsigned transaction")
	sign.reply = `{"signature":"` + base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload)) + `"}`

	routed, err := NewRoutedSigner(ctx, public, sign)
	if err != nil {
		t.Fatalf("NewRoutedSigner: %v", err)
	}

	// The worker asks to sign once, then reports what it got back.
	var got string
	asked := false
	worker := stubTool{
		name: "mint",
		invoke: func(_ context.Context, input json.RawMessage) (string, error) {
			if !asked {
				asked = true
				return `{"session":"s1","sign":{"message":"` +
					base64.StdEncoding.EncodeToString(payload) + `"}}`, nil
			}
			var resumed struct {
				Signature string `json:"signature"`
			}
			if uerr := json.Unmarshal(input, &resumed); uerr != nil {
				return "", uerr
			}
			got = resumed.Signature
			return "minted", nil
		},
	}

	h, _, m := mountStub(t, []mission.Tool{worker},
		WithHostSigner(func(string, string) HostSigner { return routed }))
	// Deliberately NO WithSignPolicy: the signer is the one that looks.

	out, err := h.Tools(m.ID)[0].Invoke(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("the routed signing call failed: %v", err)
	}
	if out != "minted" {
		t.Fatalf("terminal result = %q, want the tool's own result", out)
	}
	sig, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("the tool was resumed with a signature that is not base64: %v", err)
	}
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), payload, sig) {
		t.Fatal("the tool was resumed with a signature that does not verify over what it asked to sign")
	}
}

// TestExtensionSignerIsSelfPolicing proves the host asks no policy of a signer extension. The
// signer holds the key AND the parser, so it is the only component positioned to judge the
// payload, and requiring a second (necessarily chain-aware) opinion in the host is exactly the
// coupling this design removes.
func TestExtensionSignerIsSelfPolicing(t *testing.T) {
	ctx := context.Background()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	public, sign := signerPair(t, key)
	sign.reply = `{"signature":"` + base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte("x"))) + `"}`

	signer, err := NewRoutedSigner(ctx, public, sign)
	if err != nil {
		t.Fatalf("NewRoutedSigner: %v", err)
	}
	if !selfPoliced(signer) {
		t.Fatal("a signer extension must police its own payloads, or the host would have to parse them and the key might as well have stayed here")
	}
}

// TestExtensionSignerRefusalYieldsNoSignature is the security property. A signer that refuses
// (an unsafe transaction, by its own rules) must produce NO signature, and the refusal must
// reach the caller rather than being swallowed into an empty one.
func TestExtensionSignerRefusalYieldsNoSignature(t *testing.T) {
	ctx := context.Background()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	public, sign := signerPair(t, key)
	sign.err = errors.New("mint authority is not revoked in this transaction")

	signer, err := NewRoutedSigner(ctx, public, sign)
	if err != nil {
		t.Fatalf("NewRoutedSigner: %v", err)
	}
	sig, err := signer.Sign(ctx, []byte("a draining transaction"))
	if err == nil {
		t.Fatal("the signer refused the payload but the host produced a signature anyway")
	}
	if len(sig) != 0 {
		t.Fatal("a refusal returned signature bytes")
	}
	if !strings.Contains(err.Error(), "mint authority is not revoked") {
		t.Fatalf("the signer's reason for refusing did not reach the caller: %v", err)
	}
}

// TestExtensionSignerRejectsAnEmptySignature: a signer that approves but hands back nothing
// must not be read as a successful signature over nothing. The tool result is untrusted, so an
// empty or malformed signature is a failure, never an empty success.
func TestExtensionSignerRejectsAnEmptySignature(t *testing.T) {
	ctx := context.Background()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))

	for name, reply := range map[string]string{
		"empty signature":  `{"signature":""}`,
		"absent signature": `{}`,
		"not base64":       `{"signature":"@@@@"}`,
		"not json":         `i approve`,
	} {
		t.Run(name, func(t *testing.T) {
			public, sign := signerPair(t, key)
			sign.reply = reply
			signer, err := NewRoutedSigner(ctx, public, sign)
			if err != nil {
				t.Fatalf("NewRoutedSigner: %v", err)
			}
			if _, err := signer.Sign(ctx, []byte("x")); err == nil {
				t.Fatal("a signer that returned no usable signature was treated as a success")
			}
		})
	}
}

// TestExtensionSignerFailsAtMount: a signer that cannot be reached, or that answers with no
// key, fails when it is mounted rather than halfway through a mint. A worker must never be
// wired to a signer that cannot sign.
func TestExtensionSignerFailsAtMount(t *testing.T) {
	ctx := context.Background()

	t.Run("signer unreachable", func(t *testing.T) {
		public := &stubSignerTool{name: SignerPublicTool, err: errors.New("no such process")}
		if _, err := NewRoutedSigner(ctx, public, &stubSignerTool{name: SignerSignTool}); err == nil {
			t.Fatal("mounted against a signer that cannot be reached")
		}
	})
	t.Run("no key", func(t *testing.T) {
		public := &stubSignerTool{name: SignerPublicTool, reply: `{"publicKey":"","curve":"ed25519"}`}
		if _, err := NewRoutedSigner(ctx, public, &stubSignerTool{name: SignerSignTool}); err == nil {
			t.Fatal("mounted against a signer that has no key")
		}
	})
	t.Run("missing tool", func(t *testing.T) {
		if _, err := NewRoutedSigner(ctx, nil, &stubSignerTool{name: SignerSignTool}); err == nil {
			t.Fatal("mounted against a signer missing its public-key tool")
		}
	})
}

// TestUnpolicedHostHeldKeyStillRefuses guards the property the route must not weaken. A key
// held HERE, with nobody looking at the payload, still signs nothing: SelfPolicing is a claim
// only a signer that actually holds the key and the parser gets to make, and it must not
// become a way to switch the host's own check off.
func TestUnpolicedHostHeldKeyStillRefuses(t *testing.T) {
	askToSign := `{"session":"s","sign":{"message":"` + base64.StdEncoding.EncodeToString([]byte("x")) + `"}}`
	stub := stubTool{
		name: "mint",
		invoke: func(context.Context, json.RawMessage) (string, error) {
			return askToSign, nil
		},
	}
	h, _, m := mountStub(t, []mission.Tool{stub},
		WithHostSigner(func(string, string) HostSigner { return testSigner(t) }))
	// No WithSignPolicy: the host holds a key and has been given nothing to judge with.

	_, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "extension_sign_unpoliced") {
		t.Fatalf("a host-held key with no policy signed something: %v", err)
	}
}

func mustSign(ctx context.Context, t *testing.T, s HostSigner, payload []byte) []byte {
	t.Helper()
	sig, err := s.Sign(ctx, payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return sig
}
