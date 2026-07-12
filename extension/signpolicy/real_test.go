package signpolicy

import (
	"os"
	"testing"
)

// TestApprovesTheRealDevnetMint runs the policy against the EXACT bytes of a transaction that
// minted a real token on devnet (5y2hYMrq...), pulled back off the chain. The synthetic tests
// above prove the policy refuses what it should; this one proves it does not refuse what it
// must not, because a policy that rejects the legitimate mint is just a broken mint with extra
// steps. It also catches the case where the tests and the parser share the same misreading of
// the message format: these bytes were produced by the Solana library, not by this package.
func TestApprovesTheRealDevnetMint(t *testing.T) {
	msg, err := os.ReadFile("testdata/real_mint_message.bin")
	if err != nil {
		t.Skip("no recorded mainnet-shaped message; skipping")
	}
	m, err := parseMessage(msg)
	if err != nil {
		t.Fatalf("could not parse a real Solana message the chain accepted: %v", err)
	}
	if len(m.accounts) == 0 {
		t.Fatal("no accounts")
	}
	// Account 0 of a Solana message is the fee payer, which for this transaction is the
	// devnet payer that signed it.
	p := Solana{Payer: m.accounts[0]}
	if err := p.Approve(msg); err != nil {
		t.Fatalf("the policy REFUSED a real, legitimate atomic mint that the chain accepted: %v", err)
	}
}
