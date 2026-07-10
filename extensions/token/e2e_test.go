//go:build token

package token

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/ionalpha/flynn/mission"
)

// TestE2EMintThroughTool drives the full guarded mint through the mission.Tool path
// against devnet: tool.Invoke -> engine.Mint -> safety.Guard -> create/metadata/
// supply/revoke -> on-chain verify. It is skipped unless SOLANA_DEVNET_E2E is set
// and KEYPAIR points at a funded devnet keypair, because it spends devnet SOL.
func TestE2EMintThroughTool(t *testing.T) {
	if os.Getenv("SOLANA_DEVNET_E2E") == "" {
		t.Skip("set SOLANA_DEVNET_E2E=1 and KEYPAIR=<path> to run the devnet e2e")
	}
	payerKey, err := solana.PrivateKeyFromSolanaKeygenFile(os.Getenv("KEYPAIR"))
	if err != nil {
		t.Fatalf("load keypair: %v", err)
	}
	e := NewEngine(rpc.New(rpc.DevNet_RPC), KeySigner{Key: payerKey})

	var mintTl mission.Tool
	for _, tl := range Tools(e) {
		if tl.Def().Name == "token_mint" {
			mintTl = tl
		}
	}
	if mintTl == nil {
		t.Fatal("token_mint tool not found")
	}

	input := json.RawMessage(`{"name":"Flynn","symbol":"FLYNN","metadataUri":"https://flynnhq.com/flynn-token.json","decimals":9,"supply":1000000000}`)
	out, err := mintTl.Invoke(context.Background(), input)
	if err != nil {
		t.Fatalf("mint via tool: %v", err)
	}
	t.Logf("RESULT: %s", out)
	if !strings.Contains(out, "mint authority revoked") || !strings.Contains(out, "no freeze authority") {
		t.Fatalf("result did not confirm a safe token: %s", out)
	}
}
