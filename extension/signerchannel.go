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

// SignerChannel is the host's private line to a signer extension. The
// signer's tools are never mounted: a model that could call them would unlock
// the key or sign without the worker, and no grant on the worker stops that.
// Only the operator's passphrase and the worker's payload cross it.
type SignerChannel interface {
	// Unlock returns the public key and its curve. A signer's own --key
	// overrides keyPath.
	Unlock(ctx context.Context, passphrase secret.Text, keyPath string) (pub []byte, curve string, err error)
	SignPayload(ctx context.Context, payload []byte) ([]byte, error)
}

// Reserved signer tool names: only the host calls them, so they are never
// mounted.
const (
	// SignerUnlockTool is called once, at mount, with the operator's
	// passphrase. Until it succeeds the signer can sign nothing.
	SignerUnlockTool = "signer_unlock"
	// SignerSignTool signs a payload or refuses it under the signer's policy.
	SignerSignTool = "signer_sign"
)

func reservedSignerTool(name string) bool {
	return name == SignerUnlockTool || name == SignerSignTool
}

// mcpSignerChannel calls the signer's MCP client directly, so the secret and
// payload stay off the shared tool dispatch.
type mcpSignerChannel struct {
	client  *mcp.Client
	timeout time.Duration
}

// unlockReply carries the base64 public key and curve, both opaque to the
// host.
type unlockReply struct {
	PublicKey string `json:"publicKey"`
	Curve     string `json:"curve"`
}

// signReply is the approval reply. A refusal is a tool error, so it cannot be
// read as an empty signature.
type signReply struct {
	Signature string `json:"signature"`
}

// Unlock sends the passphrase and keyPath, omitted when empty.
func (c *mcpSignerChannel) Unlock(ctx context.Context, passphrase secret.Text, keyPath string) ([]byte, string, error) {
	if passphrase.Empty() {
		return nil, "", fault.New(fault.Terminal, "extension_signer_unlock",
			"extension: no passphrase for the signer, so its key cannot be unlocked")
	}
	// The passphrase leaves the host here, to the only process that can use
	// it, over a channel no tool call reaches.
	fields := map[string]string{"passphrase": passphrase.Expose()}
	if keyPath != "" {
		fields["keyPath"] = keyPath
	}
	req, err := json.Marshal(fields)
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

// SignPayload returns the signature, or the signer's refusal unchanged.
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

// call returns a tool-level error, such as a refusal, as an error.
func (c *mcpSignerChannel) call(ctx context.Context, tool string, req json.RawMessage, out any) error {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	res, err := c.client.CallTool(callCtx, tool, req)
	if err != nil {
		return fault.Wrap(fault.Transient, "extension_signer_call", err)
	}
	if res.IsError {
		// The refusal text names the rule, so it is kept verbatim.
		return fault.New(fault.Forbidden, "extension_signer_refused", strings.TrimSpace(res.Text))
	}
	if err := json.Unmarshal([]byte(res.Text), out); err != nil {
		return fault.Wrap(fault.Terminal, "extension_signer_reply", err)
	}
	return nil
}

var _ SignerChannel = (*mcpSignerChannel)(nil)

// SignerChannelFor fails unless the signer is mounted, so a worker is never
// wired to a signer that is not running.
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
