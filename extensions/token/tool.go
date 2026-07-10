//go:build token

package token

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	solana "github.com/gagliardetto/solana-go"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
)

// Tools returns the token capability's agent-callable tools bound to an engine. A
// host mounts these only when the token capability is granted, so the default agent
// never sees them.
func Tools(e *Engine) []mission.Tool {
	return []mission.Tool{mintTool{e}, verifyTool{e}}
}

// mintTool mints a new fixed-supply token through the guarded lifecycle. It exposes
// no scam levers: the only inputs are identity and supply, and the safety policy
// runs before anything is signed.
type mintTool struct{ e *Engine }

func (mintTool) Def() llm.Tool {
	return llm.Tool{
		Name: "token_mint",
		Description: "Mint a new fixed-supply SPL token safely on Solana: create the mint, attach " +
			"metadata, mint the whole supply, then revoke the mint authority. Freeze authority is never " +
			"set. The request is refused if it would produce a scam-shaped token. Returns the mint address.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"name":{"type":"string"},"symbol":{"type":"string"},"metadataUri":{"type":"string"},` +
			`"decimals":{"type":"integer"},"supply":{"type":"integer"}},` +
			`"required":["name","symbol","metadataUri","supply"]}`),
	}
}

func (t mintTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Name        string `json:"name"`
		Symbol      string `json:"symbol"`
		MetadataURI string `json:"metadataUri"`
		Decimals    uint8  `json:"decimals"`
		Supply      uint64 `json:"supply"`
	}
	a.Decimals = 9
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("token_mint: bad input: %w", err)
	}
	mint, disclosures, err := t.e.Mint(ctx, MintSpec{Name: a.Name, Symbol: a.Symbol, MetadataURI: a.MetadataURI, Decimals: a.Decimals, Supply: a.Supply})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "minted safe token %s (name=%q symbol=%q supply=%d, decimals=%d): mint authority revoked, no freeze authority",
		mint, a.Name, a.Symbol, a.Supply, a.Decimals)
	for _, d := range disclosures {
		fmt.Fprintf(&b, "\ndisclosure (%s): %s", d.Code, d.Detail)
	}
	return b.String(), nil
}

// verifyTool reports a mint's scam-relevant state.
type verifyTool struct{ e *Engine }

func (verifyTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "token_verify",
		Description: "Verify a Solana mint: report supply, decimals, and whether the mint and freeze authorities are revoked (a safe token has both revoked).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"mint":{"type":"string"}},"required":["mint"]}`),
	}
}

func (t verifyTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Mint string `json:"mint"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("token_verify: bad input: %w", err)
	}
	pk, err := solana.PublicKeyFromBase58(a.Mint)
	if err != nil {
		return "", fmt.Errorf("token_verify: bad mint address: %w", err)
	}
	st, err := t.e.Verify(ctx, pk)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("mint %s: supply=%d decimals=%d mintAuthorityRevoked=%t freezeAbsent=%t",
		st.Mint, st.Supply, st.Decimals, st.SupplyFixed(), !st.Freezable()), nil
}
