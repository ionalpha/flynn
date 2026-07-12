package signpolicy

import (
	"errors"
	"fmt"
)

// A Solana transaction message, as it appears on the wire:
//
//	header            3 bytes: numRequiredSignatures, numReadonlySigned, numReadonlyUnsigned
//	accounts          compact-u16 count, then that many 32-byte addresses
//	recentBlockhash   32 bytes
//	instructions      compact-u16 count, then for each:
//	                    programIDIndex  u8       (an index into accounts)
//	                    accountIndexes  compact-u16 count, then that many u8
//	                    data            compact-u16 length, then that many bytes
//
// That is the whole format. Reading it requires no cryptography and no chain library, which
// is what lets a deliberately crypto-free host inspect what it is about to sign.

const addrLen = 32

type instruction struct {
	programIndex uint8
	data         []byte
}

type message struct {
	accounts     [][]byte
	instructions []instruction
}

// programOf resolves the program an instruction invokes. An index outside the account list is
// a malformed message, and a malformed message is refused rather than guessed at.
func (m message) programOf(ix instruction) ([]byte, error) {
	if int(ix.programIndex) >= len(m.accounts) {
		return nil, errors.New("names a program account that the message does not contain")
	}
	return m.accounts[ix.programIndex], nil
}

// parseMessage reads a legacy Solana message.
//
// A versioned (v0) message sets the high bit of the first byte. Those carry address lookup
// tables, which means some of the accounts an instruction touches are NOT in the message at
// all: they are loaded from an on-chain table this parser cannot see. A policy cannot make
// honest promises about a message whose accounts it cannot enumerate, so a versioned message
// is refused outright rather than approved on partial information.
func parseMessage(b []byte) (message, error) {
	r := &reader{b: b}

	if len(b) == 0 {
		return message{}, errors.New("empty payload")
	}
	if b[0]&0x80 != 0 {
		return message{}, errors.New("a versioned message, whose accounts may come from an address lookup table this host cannot read; refusing to approve what it cannot see")
	}

	if _, err := r.take(3); err != nil { // header
		return message{}, fmt.Errorf("header: %w", err)
	}

	nAccounts, err := r.compactU16()
	if err != nil {
		return message{}, fmt.Errorf("account count: %w", err)
	}
	m := message{accounts: make([][]byte, 0, nAccounts)}
	for i := range nAccounts {
		a, err := r.take(addrLen)
		if err != nil {
			return message{}, fmt.Errorf("account %d: %w", i, err)
		}
		m.accounts = append(m.accounts, a)
	}

	if _, err := r.take(addrLen); err != nil { // recent blockhash
		return message{}, fmt.Errorf("blockhash: %w", err)
	}

	nIxs, err := r.compactU16()
	if err != nil {
		return message{}, fmt.Errorf("instruction count: %w", err)
	}
	for i := range nIxs {
		progIdx, err := r.byte()
		if err != nil {
			return message{}, fmt.Errorf("instruction %d program index: %w", i, err)
		}
		nAccts, err := r.compactU16()
		if err != nil {
			return message{}, fmt.Errorf("instruction %d account count: %w", i, err)
		}
		if _, err := r.take(nAccts); err != nil {
			return message{}, fmt.Errorf("instruction %d accounts: %w", i, err)
		}
		nData, err := r.compactU16()
		if err != nil {
			return message{}, fmt.Errorf("instruction %d data length: %w", i, err)
		}
		data, err := r.take(nData)
		if err != nil {
			return message{}, fmt.Errorf("instruction %d data: %w", i, err)
		}
		m.instructions = append(m.instructions, instruction{programIndex: progIdx, data: data})
	}

	// Trailing bytes mean this is not the message we think it is. Refuse rather than approve
	// a prefix of something else.
	if r.remaining() != 0 {
		return message{}, fmt.Errorf("%d trailing bytes after the message", r.remaining())
	}
	return m, nil
}

// reader walks the payload without ever indexing past its end.
type reader struct {
	b   []byte
	off int
}

func (r *reader) remaining() int { return len(r.b) - r.off }

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || r.remaining() < n {
		return nil, errors.New("truncated")
	}
	out := r.b[r.off : r.off+n]
	r.off += n
	return out, nil
}

func (r *reader) byte() (uint8, error) {
	b, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// compactU16 reads Solana's ShortVec length: up to three bytes, seven bits each, little end
// first, with the high bit marking continuation.
func (r *reader) compactU16() (int, error) {
	var val, shift int
	for i := range 3 {
		b, err := r.byte()
		if err != nil {
			return 0, err
		}
		val |= int(b&0x7f) << shift
		if b&0x80 == 0 {
			if i > 0 && b == 0 {
				return 0, errors.New("non-canonical length encoding")
			}
			if val > 0xffff {
				return 0, errors.New("length out of range")
			}
			return val, nil
		}
		shift += 7
	}
	return 0, errors.New("length is not terminated")
}

// mustAddr decodes a base58 address at init. It exists so the program IDs above can be
// written the way everyone reads them, while the policy still compares raw bytes.
func mustAddr(s string) []byte {
	b, err := base58Decode(s)
	if err != nil || len(b) != addrLen {
		panic("signpolicy: bad program address " + s)
	}
	return b
}

const b58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Decode is a small bignum-free base58 decoder, present so this package pulls in no
// dependency at all. It runs four times, at init.
func base58Decode(s string) ([]byte, error) {
	out := make([]byte, 0, addrLen)
	for _, c := range []byte(s) {
		idx := -1
		for i := range len(b58Alphabet) {
			if b58Alphabet[i] == c {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, errors.New("not base58")
		}
		carry := idx
		for i := range out {
			carry += 58 * int(out[i])
			out[i] = byte(carry % 256)
			carry /= 256
		}
		for carry > 0 {
			out = append(out, byte(carry%256))
			carry /= 256
		}
	}
	// Leading '1's are leading zero bytes.
	for i := 0; i < len(s) && s[i] == '1'; i++ {
		out = append(out, 0)
	}
	// The accumulator is little-endian; the address is big-endian.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
