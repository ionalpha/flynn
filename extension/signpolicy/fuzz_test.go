package signpolicy

import (
	"bytes"
	"os"
	"testing"
)

// FuzzApprove throws arbitrary bytes at the policy. This is the target that matters most in
// this package: Approve runs on the payload a mounted extension hands the host, and a mounted
// extension is treated as potentially compromised. So the input is adversarial by definition,
// and the two failure modes that must never happen are:
//
//   - a panic, which is a denial of service on the host triggered by untrusted input;
//   - approving something the parser only partially understood.
//
// The fuzzer cannot check the second directly, but the parser is written so that anything it
// cannot fully account for (a truncation, a trailing byte, a versioned message, a length it
// cannot place) is an error, and Approve turns any parse error into a refusal. So the property
// the fuzzer enforces is simpler and total: Approve either refuses or approves without ever
// panicking, for every possible input.
func FuzzApprove(f *testing.F) {
	// Seed with the shapes the corpus should explore around: a real mint, and the hand-built
	// attacks, so the fuzzer starts from valid structure and mutates outward.
	f.Add(goodMint())
	f.Add([]byte{})
	f.Add([]byte{0x80})                   // the versioned-message bit alone
	f.Add(bytes.Repeat([]byte{0xff}, 64)) // dense high bytes
	f.Add(make([]byte, 1024))             // all zeros
	if corpus, err := readCorpusMint(); err == nil {
		f.Add(corpus)
	}

	p := Solana{Payer: bytes.Repeat([]byte{0xAA}, 32)}
	f.Fuzz(func(t *testing.T, payload []byte) {
		// The contract: never panic. A refusal is fine, an approval is fine; a panic is a
		// host DoS. Any approval must at least have parsed cleanly, which we re-check to make
		// sure "approved" never coexists with "unparseable".
		err := p.Approve(payload)
		if err == nil {
			if _, perr := parseMessage(payload); perr != nil {
				t.Fatalf("Approve accepted a payload the parser rejects: %v", perr)
			}
		}
	})
}

// FuzzParseMessage targets the parser directly, so a mutation that survives to produce a
// message is exercised against the accessor that reads it. A parsed message must never let
// programOf index out of range, and its account bytes must all be full addresses.
func FuzzParseMessage(f *testing.F) {
	f.Add(goodMint())
	f.Fuzz(func(t *testing.T, payload []byte) {
		m, err := parseMessage(payload)
		if err != nil {
			return
		}
		for _, a := range m.accounts {
			if len(a) != addrLen {
				t.Fatalf("parsed an account that is not %d bytes: %d", addrLen, len(a))
			}
		}
		for _, ix := range m.instructions {
			// Must not panic or index out of range whatever the program index is.
			_, _ = m.programOf(ix)
		}
	})
}

func readCorpusMint() ([]byte, error) {
	return os.ReadFile("testdata/real_mint_message.bin")
}
