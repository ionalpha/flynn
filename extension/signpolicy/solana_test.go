package signpolicy

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// These tests are written from the attacker's side. Each one is a payload a compromised token
// extension could hand the host, having already passed every check that happens before the
// key is used: the binary's signature verified, the sandbox held, the extension's own safety
// policy was whatever the attacker wanted it to be. The only thing left is whether the host
// looks at what it signs.

var payer = bytes.Repeat([]byte{0xAA}, 32)

func policy() Solana { return Solana{Payer: payer} }

// msg builds a legacy Solana message with the given accounts and instructions.
func msg(accounts [][]byte, ixs ...instruction) []byte {
	var b bytes.Buffer
	b.Write([]byte{1, 0, 0}) // header
	b.Write(shortVec(len(accounts)))
	for _, a := range accounts {
		b.Write(a)
	}
	b.Write(make([]byte, 32)) // blockhash
	b.Write(shortVec(len(ixs)))
	for _, ix := range ixs {
		b.WriteByte(ix.programIndex)
		b.Write(shortVec(0)) // no account indexes; the policy does not read them
		b.Write(shortVec(len(ix.data)))
		b.Write(ix.data)
	}
	return b.Bytes()
}

func shortVec(n int) []byte {
	var out []byte
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(out, c)
		}
		out = append(out, c|0x80)
	}
}

func sysIx(kind uint32) []byte {
	d := make([]byte, 4)
	binary.LittleEndian.PutUint32(d, kind)
	return d
}

// A well-formed mint: create metadata, create the token account, mint the supply, revoke both
// authorities. This is what the real engine builds, and it must be approved.
func goodMint() []byte {
	accts := [][]byte{payer, metadataProgram, ataProgram, tokenProgram}
	return msg(accts,
		instruction{programIndex: 1, data: []byte{42}},                                       // Metaplex Create
		instruction{programIndex: 2, data: []byte{1}},                                        // ATA CreateIdempotent
		instruction{programIndex: 3, data: []byte{splMintTo, 0, 0, 0, 0, 0, 0, 0, 0}},        // MintTo
		instruction{programIndex: 3, data: []byte{splSetAuthority, authorityFreezeAccnt, 0}}, // revoke freeze
		instruction{programIndex: 3, data: []byte{splSetAuthority, authorityMintTokens, 0}},  // revoke mint
	)
}

func TestApprovesTheLegitimateMint(t *testing.T) {
	if err := policy().Approve(goodMint()); err != nil {
		t.Fatalf("the real atomic mint was refused: %v", err)
	}
}

// TestRefusesAMintThatKeepsTheMintAuthority is the invariant that matters most. A compromised
// extension asks for a mint it can inflate forever afterwards. The extension's own safety
// policy would have refused this, but the extension is the thing that was compromised, so its
// refusal is worth nothing. The host has to be the one that says no.
func TestRefusesAMintThatKeepsTheMintAuthority(t *testing.T) {
	accts := [][]byte{payer, tokenProgram}
	m := msg(accts,
		instruction{programIndex: 1, data: []byte{splMintTo, 0, 0, 0, 0, 0, 0, 0, 0}},
		instruction{programIndex: 1, data: []byte{splSetAuthority, authorityFreezeAccnt, 0}},
	)
	err := policy().Approve(m)
	if err == nil {
		t.Fatal("signed a mint whose mint authority is never revoked: the supply could be inflated forever")
	}
	if !strings.Contains(err.Error(), "revoke the mint authority") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestRefusesAMintThatKeepsTheFreezeAuthority: the honeypot. Holders can be frozen out of
// selling.
func TestRefusesAMintThatKeepsTheFreezeAuthority(t *testing.T) {
	accts := [][]byte{payer, tokenProgram}
	m := msg(accts,
		instruction{programIndex: 1, data: []byte{splMintTo, 0, 0, 0, 0, 0, 0, 0, 0}},
		instruction{programIndex: 1, data: []byte{splSetAuthority, authorityMintTokens, 0}},
	)
	err := policy().Approve(m)
	if err == nil {
		t.Fatal("signed a mint that retains a freeze authority: holders could be frozen out of selling")
	}
	if !strings.Contains(err.Error(), "freeze authority") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestRefusesDrainingTheKey is the plainest attack: the extension asks the host to sign away
// the payer's SOL. The key pays fees and rent; it does not make transfers.
func TestRefusesDrainingTheKey(t *testing.T) {
	attacker := bytes.Repeat([]byte{0xBB}, 32)
	m := msg([][]byte{payer, attacker, systemProgram},
		instruction{programIndex: 2, data: append(sysIx(systemTransfer), make([]byte, 8)...)})
	err := policy().Approve(m)
	if err == nil {
		t.Fatal("signed a System::Transfer: a compromised extension could drain the key")
	}
	if !strings.Contains(err.Error(), "Transfer") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestRefusesHandingTheMintAuthorityToSomebodyElse: SetAuthority with a NEW authority rather
// than None. It looks like a revoke to anything that only counts SetAuthority instructions.
func TestRefusesHandingTheMintAuthorityToAnotherKey(t *testing.T) {
	data := []byte{splSetAuthority, authorityMintTokens, 1} // COption::Some
	data = append(data, bytes.Repeat([]byte{0xBB}, 32)...)  // ... the attacker's key
	m := msg([][]byte{payer, tokenProgram},
		instruction{programIndex: 1, data: []byte{splMintTo, 0, 0, 0, 0, 0, 0, 0, 0}},
		instruction{programIndex: 1, data: []byte{splSetAuthority, authorityFreezeAccnt, 0}},
		instruction{programIndex: 1, data: data},
	)
	if err := policy().Approve(m); err == nil {
		t.Fatal("signed a SetAuthority that hands the mint authority to another key instead of revoking it")
	}
}

// TestRefusesTokenTransfersAndBurns: the key is granted to mint, not to move or destroy what
// has already been minted.
func TestRefusesTokenTransfersAndBurns(t *testing.T) {
	for name, disc := range map[string]byte{"Transfer": 3, "Burn": 8, "CloseAccount": 9, "Approve": 4, "FreezeAccount": 10} {
		m := msg([][]byte{payer, tokenProgram}, instruction{programIndex: 1, data: []byte{disc, 0}})
		if err := policy().Approve(m); err == nil {
			t.Errorf("signed an SPL Token %s", name)
		}
	}
}

// TestRefusesAnUnknownProgram: the key must not be usable against a program it was never
// granted for, which is every program that is not part of minting a token.
func TestRefusesAnUnknownProgram(t *testing.T) {
	stranger := bytes.Repeat([]byte{0xCC}, 32)
	m := msg([][]byte{payer, stranger}, instruction{programIndex: 1, data: []byte{0}})
	if err := policy().Approve(m); err == nil {
		t.Fatal("signed an instruction against an unknown program")
	}
}

// TestRefusesWhenTheFeePayerIsNotTheGrantedKey: the extension gets this key to co-sign a
// transaction whose fee payer, and therefore whose shape and authority, is somebody else's.
func TestRefusesWhenTheFeePayerIsNotTheGrantedKey(t *testing.T) {
	other := bytes.Repeat([]byte{0xDD}, 32)
	m := msg([][]byte{other, payer, tokenProgram},
		instruction{programIndex: 2, data: []byte{splSetAuthority, authorityMintTokens, 0}})
	if err := policy().Approve(m); err == nil {
		t.Fatal("signed a message whose fee payer is not the granted key")
	}
}

// TestRefusesAVersionedMessage: a v0 message can load accounts from an on-chain lookup table
// that is not in the payload. A policy cannot honestly approve a message whose accounts it
// cannot enumerate, so it refuses rather than approving on partial information.
func TestRefusesAVersionedMessage(t *testing.T) {
	m := goodMint()
	m[0] |= 0x80
	err := policy().Approve(m)
	if err == nil {
		t.Fatal("approved a versioned message whose accounts may come from a lookup table it cannot see")
	}
	if !strings.Contains(err.Error(), "versioned") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestRefusesMalformedPayloads: anything the parser cannot read is refused, never guessed at.
func TestRefusesMalformedPayloads(t *testing.T) {
	good := goodMint()
	cases := map[string][]byte{
		"empty":            {},
		"truncated":        good[:len(good)/2],
		"trailing garbage": append(append([]byte{}, good...), 0xFF),
		"not a message":    bytes.Repeat([]byte{0x11}, 40),
	}
	for name, payload := range cases {
		if err := policy().Approve(payload); err == nil {
			t.Errorf("approved a %s payload", name)
		}
	}
}

// TestProgramIndexAtTheBoundaryIsRefused pins the exact off-by-one in programOf: a program
// index EQUAL to the account count is out of range (valid indices are 0..len-1), and must be a
// clean refusal, never an out-of-bounds panic. Without this case the bounds check `>=` reads
// the same as `>`, which would step one past the end.
func TestProgramIndexAtTheBoundaryIsRefused(t *testing.T) {
	accounts := [][]byte{payer, tokenProgram} // len 2: valid indices are 0 and 1
	// An instruction whose program index is exactly len(accounts) = 2, one past the end.
	m := msg(accounts, instruction{programIndex: 2, data: []byte{splMintTo}})
	if err := policy().Approve(m); err == nil {
		t.Fatal("a program index one past the last account was accepted; it must be refused, not read out of bounds")
	}

	// And the parser's accessor must not panic on that index directly.
	parsed, err := parseMessage(m)
	if err != nil {
		t.Fatalf("message did not parse: %v", err)
	}
	if _, err := parsed.programOf(instruction{programIndex: uint8(len(accounts))}); err == nil {
		t.Fatal("programOf accepted an index equal to the account count")
	}
}
