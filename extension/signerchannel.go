package extension

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/mcp"
	"github.com/ionalpha/flynn/secret"
)

// SignerChannel is the host's private line to a signer extension. It is deliberately NOT the
// agent's tool surface.
//
// A mounted tool is a tool the MODEL can call. If a signer's tools were mounted, the model
// could unlock the key itself, or call the signing tool directly and skip the worker that was
// supposed to build the transaction. Neither is a thing an agent should be able to do, and
// neither is stopped by a capability grant on the worker, because the model would not be going
// through the worker at all.
//
// So the signer's tools are never mounted (see reservedSignerTool). The host reaches them over
// this channel, which nothing model-facing can get at, and the only thing that ever crosses it
// is a passphrase the operator granted and a payload the worker built.
type SignerChannel interface {
	// Unlock opens the signer's key and returns its public half and the curve it sits on.
	Unlock(ctx context.Context, passphrase secret.Text) (pub []byte, curve string, err error)
	// SignPayload asks the signer to sign payload. The signer parses it and may refuse.
	SignPayload(ctx context.Context, payload []byte) ([]byte, error)
}

// signer tool names. They are RESERVED: an extension advertising them does not get them
// mounted, because they are the host's to call and nobody else's.
const (
	// SignerUnlockTool opens the signer's key and answers with its public half. The host
	// calls it once, at mount, with a passphrase the OPERATOR holds (in the vault). Until it
	// succeeds the signer holds nothing and can sign nothing.
	SignerUnlockTool = "signer_unlock"
	// SignerSignTool signs a payload, or refuses it. The signer applies its own policy here.
	SignerSignTool = "signer_sign"
)

// reservedSignerTool reports whether a tool name belongs to the host's signer channel. A tool
// with one of these names is never mounted for the model: it is reachable only from the host,
// over SignerChannel.
func reservedSignerTool(name string) bool {
	return name == SignerUnlockTool || name == SignerSignTool
}

// mcpSignerChannel speaks to a signer extension over its MCP client directly, rather than
// through a mounted mission.Tool. That is the point: the secret and the payload travel a
// concrete, host-only path, not the generic tool-dispatch surface every other extension shares.
type mcpSignerChannel struct {
	client  *mcp.Client
	timeout time.Duration
}

// unlockReply is what SignerUnlockTool returns: the public half, base64, plus the curve it
// sits on. The host copies the key bytes through to the worker and interprets neither.
type unlockReply struct {
	PublicKey string `json:"publicKey"`
	Curve     string `json:"curve"`
}

// signReply is what SignerSignTool returns on approval. A refusal comes back as a tool error
// naming the rule that failed, not as a reply with an empty signature, so a refusal can never
// be mistaken for a successful signature over nothing.
type signReply struct {
	Signature string `json:"signature"`
}

// Unlock hands the operator's passphrase to the signer and takes back its public key.
func (c *mcpSignerChannel) Unlock(ctx context.Context, passphrase secret.Text) ([]byte, string, error) {
	if passphrase.Empty() {
		return nil, "", fault.New(fault.Terminal, "extension_signer_unlock",
			"extension: no passphrase for the signer, so its key cannot be unlocked")
	}
	// Expose is the audited point where a secret crosses a boundary the host does not control.
	// This is one, and it is a narrow one: the passphrase goes to the signer subprocess, which
	// is the only party that can do anything with it, over a channel no tool call can reach.
	req, err := json.Marshal(map[string]string{"passphrase": passphrase.Expose()})
	if err != nil {
		return nil, "", fault.Wrap(fault.Terminal, "extension_signer_unlock", err)
	}
	var reply unlockReply
	if err := c.call(ctx, SignerUnlockTool, req, &reply); err != nil {
		return nil, "", fault.Wrap(fault.Transient, "extension_signer_unlock", err)
	}
	pub, err := base64.StdEncoding.DecodeString(reply.PublicKey)
	if err != nil || len(pub) == 0 {
		return nil, "", fault.New(fault.Terminal, "extension_signer_unlock",
			"extension: signer returned no usable public key")
	}
	return pub, reply.Curve, nil
}

// SignPayload asks the signer to sign, and returns its refusal untouched if it will not. The
// reason belongs to the operator reading the error; this host cannot check the claim and does
// not try.
func (c *mcpSignerChannel) SignPayload(ctx context.Context, payload []byte) ([]byte, error) {
	req := json.RawMessage(`{"payload":"` + base64.StdEncoding.EncodeToString(payload) + `"}`)
	var reply signReply
	if err := c.call(ctx, SignerSignTool, req, &reply); err != nil {
		return nil, fault.Wrap(fault.Forbidden, "extension_sign_refused", err)
	}
	sig, err := base64.StdEncoding.DecodeString(reply.Signature)
	if err != nil || len(sig) == 0 {
		return nil, fault.New(fault.Terminal, "extension_signer_reply",
			"extension: signer approved the payload but returned no signature")
	}
	return sig, nil
}

// call invokes one tool on the signer and decodes its reply. A tool-level error (the signer
// refusing) is returned as an error, never as an empty success.
func (c *mcpSignerChannel) call(ctx context.Context, tool string, req json.RawMessage, out any) error {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	res, err := c.client.CallTool(callCtx, tool, req)
	if err != nil {
		return fault.Wrap(fault.Transient, "extension_signer_call", err)
	}
	if res.IsError {
		// The signer said no. Its text names the rule it refused on, and that is the whole
		// value of the message, so it is carried out verbatim.
		return fault.New(fault.Forbidden, "extension_signer_refused", strings.TrimSpace(res.Text))
	}
	if err := json.Unmarshal([]byte(res.Text), out); err != nil {
		return fault.Wrap(fault.Terminal, "extension_signer_reply", err)
	}
	return nil
}

var _ SignerChannel = (*mcpSignerChannel)(nil)

// SignerChannelFor returns the host's private line to a mounted signer extension. It fails if
// the extension is not mounted, so a worker is never wired to a signer that is not running.
func (h *ProcessHandler) SignerChannelFor(id string) (SignerChannel, error) {
	h.mu.Lock()
	p, ok := h.mounted[id]
	h.mu.Unlock()
	if !ok {
		return nil, fault.New(fault.Terminal, "extension_signer_unmounted",
			"extension: the signer extension is not mounted, so there is nothing to sign with")
	}
	return &mcpSignerChannel{client: p.client, timeout: h.callTimeout}, nil
}
