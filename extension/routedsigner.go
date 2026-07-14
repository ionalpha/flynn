package extension

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/mission"
)

// RoutedSigner is a HostSigner whose key lives in ANOTHER extension: a signer extension,
// mounted like any other, holding one chain's key and that chain's transaction parser.
//
// This is the shape that lets the host stay ignorant. A host that holds a key cannot sign
// safely without understanding what it signs, so a host that holds a key must carry a parser
// for every format it might be asked to sign, and it stops being a general engine the moment
// it does. Move the key out and the parser goes with it: the host is left routing opaque
// bytes between two processes and reading none of them.
//
// The separation is also what makes the check honest. The worker extension BUILDS the
// transaction; the signer HOLDS the key and independently decides whether to sign it. They
// are different artifacts, published and pinned separately. A worker compromised upstream
// gets no signature: it would have to compromise the signer too, and the signer is small
// enough to audit line by line and does nothing but parse, decide, and sign.
//
// What must never happen is a worker that carries its own parser and vouches for its own
// payload. That is self-policing, and it buys exactly nothing.
type RoutedSigner struct {
	sign mission.Tool
	pub  []byte
}

// signer tool names. A signer extension exposes exactly these two and nothing else.
const (
	// SignerPublicTool returns the signer's public key. The host asks once, at mount.
	SignerPublicTool = "signer_public"
	// SignerSignTool signs a payload, or refuses it. The signer applies its own policy here.
	SignerSignTool = "signer_sign"
)

// signerPublicReply is what SignerPublicTool returns: the public half, base64, plus the curve
// it sits on. The host records the curve for the operator's benefit and never acts on it: it
// copies the key bytes through to the worker and interprets neither.
type signerPublicReply struct {
	PublicKey string `json:"publicKey"`
	Curve     string `json:"curve"`
}

// signerSignReply is what SignerSignTool returns on approval. A refusal comes back as a tool
// error naming the rule that failed, not as a reply with an empty signature, so a refusal can
// never be mistaken for a successful signature over nothing.
type signerSignReply struct {
	Signature string `json:"signature"`
}

// NewRoutedSigner asks a mounted signer extension for its public key and returns a signer
// that routes signing requests to it. It fails if the signer cannot be reached or does not
// answer with a key, so a worker is never mounted against a signer that cannot sign: the
// failure lands at mount, where an operator sees it, rather than halfway through a mint.
func NewRoutedSigner(ctx context.Context, public, sign mission.Tool) (*RoutedSigner, error) {
	if public == nil || sign == nil {
		return nil, fault.New(fault.Terminal, "extension_signer_missing",
			"extension: a signer extension must expose both "+SignerPublicTool+" and "+SignerSignTool)
	}
	out, err := public.Invoke(ctx, json.RawMessage(`{}`))
	if err != nil {
		return nil, fault.Wrap(fault.Transient, "extension_signer_public", err)
	}
	var reply signerPublicReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return nil, fault.Wrap(fault.Terminal, "extension_signer_public", err)
	}
	pub, err := base64.StdEncoding.DecodeString(reply.PublicKey)
	if err != nil || len(pub) == 0 {
		return nil, fault.New(fault.Terminal, "extension_signer_public",
			"extension: signer returned no usable public key")
	}
	return &RoutedSigner{sign: sign, pub: pub}, nil
}

// Public returns the signer's public key, as the signer reported it at mount.
func (s *RoutedSigner) Public() []byte { return s.pub }

// Sign hands the payload to the signer extension and returns the detached signature it
// produced. The signer may refuse, in which case its refusal is returned as-is: it names the
// rule the payload broke, and that reason belongs to the operator reading the error, not to
// this host, which cannot check the claim and does not try.
func (s *RoutedSigner) Sign(ctx context.Context, payload []byte) ([]byte, error) {
	// base64 needs no JSON escaping, so the request is built directly. Marshalling a
	// map[string]string here could not fail, and an error branch that cannot be taken is a
	// branch nobody can test and nobody should read.
	in := json.RawMessage(`{"payload":"` + base64.StdEncoding.EncodeToString(payload) + `"}`)
	out, err := s.sign.Invoke(ctx, in)
	if err != nil {
		// The signer refused, or could not be reached. Either way no signature exists, and
		// the distinction is the signer's to explain.
		return nil, fault.Wrap(fault.Forbidden, "extension_sign_refused", err)
	}
	var reply signerSignReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return nil, fault.Wrap(fault.Terminal, "extension_signer_reply", err)
	}
	sig, err := base64.StdEncoding.DecodeString(reply.Signature)
	if err != nil || len(sig) == 0 {
		return nil, fault.New(fault.Terminal, "extension_signer_reply",
			"extension: signer approved the payload but returned no signature")
	}
	return sig, nil
}

// PolicesPayloads reports that the signer applies its own policy. It holds the key and it
// understands the format, so it is the only component positioned to judge the payload, and
// the host requires no policy of its own for it.
func (s *RoutedSigner) PolicesPayloads() bool { return true }

var (
	_ HostSigner   = (*RoutedSigner)(nil)
	_ SelfPolicing = (*RoutedSigner)(nil)
)
