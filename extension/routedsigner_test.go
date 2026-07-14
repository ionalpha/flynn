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

// stubChannel stands in for the host's private line to a signer extension: it records what it
// was asked and answers however the test wants, including a refusal.
type stubChannel struct {
	key       ed25519.PrivateKey
	unlockErr error
	signErr   error
	passSeen  secret.Text
	signed    [][]byte
}

func (c *stubChannel) Unlock(_ context.Context, passphrase secret.Text) ([]byte, string, error) {
	c.passSeen = passphrase
	if c.unlockErr != nil {
		return nil, "", c.unlockErr
	}
	if passphrase.Empty() {
		return nil, "", errors.New("no passphrase")
	}
	return pubOf(c.key), "ed25519", nil
}

func (c *stubChannel) SignPayload(_ context.Context, payload []byte) ([]byte, error) {
	c.signed = append(c.signed, payload)
	if c.signErr != nil {
		return nil, c.signErr
	}
	return ed25519.Sign(c.key, payload), nil
}

func testChannel(t *testing.T) (*stubChannel, ed25519.PrivateKey) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	return &stubChannel{key: key}, key
}

func pubOf(key ed25519.PrivateKey) ed25519.PublicKey {
	pub, _ := key.Public().(ed25519.PublicKey)
	return pub
}

// TestRoutedSignerRoutesToTheSigner proves the whole point of the route: the host gets a
// verifying signature over the exact bytes, from a key it never held.
func TestRoutedSignerRoutesToTheSigner(t *testing.T) {
	ctx := context.Background()
	ch, key := testChannel(t)

	signer, err := NewRoutedSigner(ctx, ch, secret.New("pass"))
	if err != nil {
		t.Fatalf("NewRoutedSigner: %v", err)
	}
	payload := []byte("a transaction the host cannot read")
	sig, err := signer.Sign(ctx, payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !ed25519.Verify(pubOf(key), payload, sig) {
		t.Fatal("the signature the signer returned does not verify")
	}
	// The host handed over exactly the bytes it was given, and nothing else.
	if len(ch.signed) != 1 || string(ch.signed[0]) != string(payload) {
		t.Fatalf("the host sent the signer %q, not the payload it was asked to sign", ch.signed)
	}
}

// TestRoutedSignerPublishesTheSignersKey: the host reports the signer's public key as its own
// signing identity, so a worker builds its transaction against the key that will actually sign
// it.
func TestRoutedSignerPublishesTheSignersKey(t *testing.T) {
	ch, key := testChannel(t)
	signer, err := NewRoutedSigner(context.Background(), ch, secret.New("pass"))
	if err != nil {
		t.Fatalf("NewRoutedSigner: %v", err)
	}
	if !ed25519.PublicKey(signer.Public()).Equal(pubOf(key)) {
		t.Fatal("the host advertised a key that is not the signer's")
	}
}

// TestRoutedSignerUnlocksWithTheOperatorsPassphrase: the passphrase reaches the signer, and it
// is the one the operator holds in the vault.
func TestRoutedSignerUnlocksWithTheOperatorsPassphrase(t *testing.T) {
	ch, _ := testChannel(t)
	if _, err := NewRoutedSigner(context.Background(), ch, secret.New("open sesame")); err != nil {
		t.Fatalf("NewRoutedSigner: %v", err)
	}
	if ch.passSeen.Expose() != "open sesame" {
		t.Fatal("the signer was unlocked with something other than the operator's passphrase")
	}
}

// TestRoutedSignerFailsAtMount: a signer that cannot be reached, or that gets no passphrase,
// fails when it is mounted rather than halfway through a mint. A worker must never be wired to
// a signer that cannot sign.
func TestRoutedSignerFailsAtMount(t *testing.T) {
	ctx := context.Background()

	t.Run("no channel", func(t *testing.T) {
		if _, err := NewRoutedSigner(ctx, nil, secret.New("p")); err == nil {
			t.Fatal("mounted against a signer with no channel to it")
		}
	})
	t.Run("signer unreachable", func(t *testing.T) {
		ch, _ := testChannel(t)
		ch.unlockErr = errors.New("no such process")
		if _, err := NewRoutedSigner(ctx, ch, secret.New("p")); err == nil {
			t.Fatal("mounted against a signer that cannot be reached")
		}
	})
	t.Run("empty passphrase", func(t *testing.T) {
		ch, _ := testChannel(t)
		if _, err := NewRoutedSigner(ctx, ch, secret.Text{}); err == nil {
			t.Fatal("mounted a signer with no passphrase at all")
		}
	})
}

// TestRoutedSignerIsSelfPolicing proves the host asks no policy of a signer extension. The
// signer holds the key AND the parser, so it is the only component positioned to judge the
// payload, and requiring a second (necessarily chain-aware) opinion in the host is exactly the
// coupling this design removes.
func TestRoutedSignerIsSelfPolicing(t *testing.T) {
	ch, _ := testChannel(t)
	signer, err := NewRoutedSigner(context.Background(), ch, secret.New("p"))
	if err != nil {
		t.Fatalf("NewRoutedSigner: %v", err)
	}
	if !selfPoliced(signer) {
		t.Fatal("a signer extension must police its own payloads, or the host would have to parse them and the key might as well have stayed here")
	}
}

// TestRoutedSignerRefusalYieldsNoSignature is the security property. A signer that refuses must
// produce NO signature, and the refusal must reach the caller rather than being swallowed into
// an empty one.
func TestRoutedSignerRefusalYieldsNoSignature(t *testing.T) {
	ctx := context.Background()
	ch, _ := testChannel(t)
	signer, err := NewRoutedSigner(ctx, ch, secret.New("p"))
	if err != nil {
		t.Fatalf("NewRoutedSigner: %v", err)
	}
	ch.signErr = errors.New("mint authority is not revoked in this transaction")

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

// TestRoutedSignerDrivesAMountedTool is the whole route, end to end: a worker tool asks the host
// to sign, the host routes to the signer, and the worker is resumed with a signature it can use.
// No policy is configured in the host, and none is needed: the signer polices itself, which is
// the entire reason the host no longer has to understand the payload.
func TestRoutedSignerDrivesAMountedTool(t *testing.T) {
	ctx := context.Background()
	ch, key := testChannel(t)
	routed, err := NewRoutedSigner(ctx, ch, secret.New("p"))
	if err != nil {
		t.Fatalf("NewRoutedSigner: %v", err)
	}

	payload := []byte("an unsigned transaction")
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
	if !ed25519.Verify(pubOf(key), payload, sig) {
		t.Fatal("the tool was resumed with a signature that does not verify over what it asked to sign")
	}
}

// TestSignerToolsAreNeverMountedForTheModel is the hole the private channel exists to close. A
// mounted tool is a tool the MODEL can call. If a signer's tools were mounted, the model could
// unlock the signing key itself, or ask for a signature directly and skip the worker that was
// supposed to build the transaction and be judged on it. A capability grant on the worker would
// not stop that, because the model would not be going through the worker at all.
func TestSignerToolsAreNeverMountedForTheModel(t *testing.T) {
	tools := []mission.Tool{
		stubTool{name: SignerUnlockTool, invoke: neverCalled(t)},
		stubTool{name: SignerSignTool, invoke: neverCalled(t)},
		stubTool{name: "token_verify", invoke: neverCalled(t)},
	}
	h, _, m := mountStub(t, tools)

	mounted := h.Tools(m.ID)
	for _, tool := range mounted {
		if strings.Contains(tool.Def().Name, SignerUnlockTool) || strings.Contains(tool.Def().Name, SignerSignTool) {
			t.Fatalf("the signer tool %q was mounted where the model can call it", tool.Def().Name)
		}
	}
	if len(mounted) != 1 {
		t.Fatalf("expected only the non-signer tool to mount, got %d", len(mounted))
	}
}

func neverCalled(t *testing.T) func(context.Context, json.RawMessage) (string, error) {
	return func(context.Context, json.RawMessage) (string, error) {
		t.Helper()
		t.Error("a tool that should not be mounted was invoked")
		return "", nil
	}
}

// TestASignerThatDoesNotJudgeIsRefused is the invariant that survives the host giving up its
// parser. The host now holds NO way to read a payload, so a signer that does not judge its own
// payloads leaves nobody judging them at all. That is blind signing, and it is refused rather
// than warned about.
//
// SelfPolicing is therefore a claim only a signer that really holds the key AND the parser gets
// to make, and it is not a switch anybody can flip to skip the check: refusing to implement it
// does not get you signed, it gets you refused.
func TestASignerThatDoesNotJudgeIsRefused(t *testing.T) {
	askToSign := `{"session":"s","sign":{"message":"` + base64.StdEncoding.EncodeToString([]byte("x")) + `"}}`
	stub := stubTool{
		name: "mint",
		invoke: func(context.Context, json.RawMessage) (string, error) {
			return askToSign, nil
		},
	}
	h, _, m := mountStub(t, []mission.Tool{stub},
		WithHostSigner(func(string, string) HostSigner {
			return blindSigner{pub: testSigner(t).Public()}
		}))

	_, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "extension_sign_unpoliced") {
		t.Fatalf("the host signed through a signer that reads nothing: %v", err)
	}
}
