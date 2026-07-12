package signpolicy

import (
	"bytes"
	"testing"
)

// compactU16 decides how many accounts, how many instructions, and how long each
// instruction's data is. Every field the policy reads is positioned by it, so a decoder that
// accepts a length the validator would not - or rejects one it would - is a way to hand the
// policy a different transaction than the one the chain will execute. The encoding is
// checked here directly rather than through a whole message, because the interesting inputs
// (a length of exactly 0xffff, a padded encoding of a small number) are the ones a real
// message never produces on its own.
func TestCompactU16(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want int
		err  bool
	}{
		{name: "single byte", in: []byte{0x05}, want: 5},
		{name: "single byte zero", in: []byte{0x00}, want: 0},
		{name: "largest single byte", in: []byte{0x7f}, want: 127},
		// The continuation byte carries real value here. A decoder that treated any second
		// byte as padding would reject this, and 128 accounts is a message a validator runs.
		{name: "two bytes", in: []byte{0x80, 0x01}, want: 128},
		{name: "two bytes, high", in: []byte{0xff, 0x7f}, want: 16383},
		// The ceiling, accepted. One past it, refused.
		{name: "three bytes at the maximum", in: []byte{0xff, 0xff, 0x03}, want: 0xffff},
		{name: "above the maximum", in: []byte{0xff, 0xff, 0x04}, err: true},
		// A padded encoding of a number that fits in fewer bytes. Two encodings of one
		// length is two readings of one message, so only the canonical one is a length.
		{name: "non-canonical two byte", in: []byte{0x80, 0x00}, err: true},
		{name: "non-canonical three byte", in: []byte{0x80, 0x80, 0x00}, err: true},
		{name: "never terminates", in: []byte{0x80, 0x80, 0x80}, err: true},
		{name: "truncated", in: []byte{0x80}, err: true},
		{name: "empty", in: nil, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &reader{b: tc.in}
			got, err := r.compactU16()
			switch {
			case tc.err && err == nil:
				t.Fatalf("read %d from %x; want a refusal", got, tc.in)
			case !tc.err && err != nil:
				t.Fatalf("refused %x: %v", tc.in, err)
			case !tc.err && got != tc.want:
				t.Fatalf("read %d from %x; want %d", got, tc.in, tc.want)
			}
		})
	}
}

// The program IDs the policy allows are written as base58 and compared as bytes, so this
// decoder is what makes "the token program" mean the token program. Decoding one address to
// the wrong 32 bytes would let an unlisted program pass as a listed one.
func TestBase58Decode(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []byte
		err  bool
	}{
		{name: "the first digit is zero", in: "1", want: []byte{0}},
		{name: "the second digit is one", in: "2", want: []byte{1}},
		{name: "the last digit", in: "z", want: []byte{57}},
		{name: "two digits", in: "21", want: []byte{58}},
		// 4*58 + 24 = 256: the first input that carries out of a byte, so this is the case
		// that exercises the carry and the little-to-big-endian reversal at the end.
		{name: "carries into a second byte", in: "5R", want: []byte{0x01, 0x00}},
		{name: "leading zeroes", in: "112", want: []byte{0x00, 0x00, 0x01}},
		// The System program is 32 zero bytes, which is the one address whose decoding can
		// be written down by hand.
		{name: "the system program", in: "11111111111111111111111111111111", want: make([]byte, 32)},
		// Base58 omits the four characters that look like each other. A decoder that let
		// them through would map two spellings onto one address.
		{name: "zero is not a digit", in: "0", err: true},
		{name: "capital o is not a digit", in: "O", err: true},
		{name: "capital i is not a digit", in: "I", err: true},
		{name: "lower L is not a digit", in: "l", err: true},
		{name: "a valid address with one bad character", in: "Tokenkeg0feZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := base58Decode(tc.in)
			switch {
			case tc.err && err == nil:
				t.Fatalf("decoded %q to %x; want a refusal", tc.in, got)
			case !tc.err && err != nil:
				t.Fatalf("refused %q: %v", tc.in, err)
			case !tc.err && !bytes.Equal(got, tc.want):
				t.Fatalf("decoded %q to %x; want %x", tc.in, got, tc.want)
			}
		})
	}
}

// The four addresses the policy is built from are decoded at init, where a wrong answer is
// silent. Pin their length here so a bad one is a test failure and not a program the policy
// compares against and never matches.
func TestTheProgramAddressesAreFullAddresses(t *testing.T) {
	for name, prog := range map[string][]byte{
		"system":   systemProgram,
		"token":    tokenProgram,
		"ata":      ataProgram,
		"metadata": metadataProgram,
	} {
		if len(prog) != addrLen {
			t.Errorf("the %s program decoded to %d bytes; want %d", name, len(prog), addrLen)
		}
	}
}
