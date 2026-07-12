package signpolicy

import (
	"bytes"
	"testing"

	"pgregory.net/rapid"
)

// TestApproveMatchesTheRules is a property test: it generates a message from a random sequence
// of instructions drawn from both the allowed alphabet and the forbidden one, then asserts that
// Approve's verdict matches an INDEPENDENT re-derivation of the policy from the same sequence.
// The value is that the derivation here is written differently from the checker in solana.go, so
// a bug in either that makes them disagree on some generated message surfaces as a mismatch. The
// checker cannot silently drift from the stated rules without a counterexample appearing.
func TestApproveMatchesTheRules(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		payerKey := bytes.Repeat([]byte{0xA1}, 32)
		accounts := [][]byte{
			payerKey,                       // 0: the granted key / fee payer
			systemProgram,                  // 1
			tokenProgram,                   // 2
			ataProgram,                     // 3
			metadataProgram,                // 4
			bytes.Repeat([]byte{0xEE}, 32), // 5: a stranger program
		}

		// A vocabulary of instructions, each tagged with what it means to the independent model.
		type ixSpec struct {
			ix           instruction
			transfer     bool // a System::Transfer (forbidden)
			badToken     bool // an SPL Token instruction that is neither mint nor a revoke
			mintTo       bool
			revokeMint   bool
			revokeFreeze bool
			unknownProg  bool
		}
		vocab := []ixSpec{
			{ix: instruction{programIndex: 1, data: sysData(systemCreateAccount)}},            // create account: allowed
			{ix: instruction{programIndex: 1, data: sysData(systemTransfer)}, transfer: true}, // transfer: forbidden
			{ix: instruction{programIndex: 2, data: []byte{splMintTo, 0, 0}}, mintTo: true},   // mint
			{ix: instruction{programIndex: 2, data: []byte{splSetAuthority, authorityMintTokens, 0}}, revokeMint: true},
			{ix: instruction{programIndex: 2, data: []byte{splSetAuthority, authorityFreezeAccnt, 0}}, revokeFreeze: true},
			{ix: instruction{programIndex: 2, data: []byte{splSetAuthority, authorityMintTokens, 1}}, badToken: true}, // set to a new key
			{ix: instruction{programIndex: 2, data: []byte{3, 0}}, badToken: true},                                    // SPL Transfer
			{ix: instruction{programIndex: 3, data: []byte{1}}},                                                       // ATA create: allowed
			{ix: instruction{programIndex: 4, data: []byte{42}}},                                                      // metadata: allowed
			{ix: instruction{programIndex: 5, data: []byte{0}}, unknownProg: true},                                    // stranger program
		}

		n := rapid.IntRange(0, 8).Draw(t, "n")
		var ixs []instruction
		var mints, revokeMints, revokeFreezes int
		forbidden := false
		for range n {
			spec := vocab[rapid.IntRange(0, len(vocab)-1).Draw(t, "ix")]
			ixs = append(ixs, spec.ix)
			switch {
			case spec.transfer, spec.badToken, spec.unknownProg:
				forbidden = true
			case spec.mintTo:
				mints++
			case spec.revokeMint:
				revokeMints++
			case spec.revokeFreeze:
				revokeFreezes++
			}
		}

		payload := msg(accounts, ixs...)

		// The independent verdict: the message is acceptable iff it contains no forbidden
		// instruction, and if it mints at all, it revokes both authorities.
		wantOK := !forbidden
		if mints > 0 && (revokeMints == 0 || revokeFreezes == 0) {
			wantOK = false
		}

		gotErr := (Solana{Payer: payerKey}).Approve(payload)
		if wantOK && gotErr != nil {
			t.Fatalf("Approve refused a message the rules permit: %v\n(mints=%d revokeMint=%d revokeFreeze=%d forbidden=%v)", gotErr, mints, revokeMints, revokeFreezes, forbidden)
		}
		if !wantOK && gotErr == nil {
			t.Fatalf("Approve accepted a message the rules forbid (mints=%d revokeMint=%d revokeFreeze=%d forbidden=%v)", mints, revokeMints, revokeFreezes, forbidden)
		}
	})
}

func sysData(kind uint32) []byte {
	d := make([]byte, 4)
	d[0] = byte(kind)
	d[1] = byte(kind >> 8)
	d[2] = byte(kind >> 16)
	d[3] = byte(kind >> 24)
	return d
}
