// Package signpolicy decides what a host-held key is allowed to sign on behalf of a mounted
// extension, by reading the payload instead of trusting the extension that produced it.
//
// The host grants an extension the use of a signing key. Without a policy, it then signs
// whatever bytes come back: the extension's own safety rules are the only thing standing
// between a signing key and an attacker, and those rules live inside the extension, which is
// the component being trusted. Verifying the extension's signature at install time does not
// help here. It proves the binary is the one that was published; it says nothing about what
// that binary asks to be signed at runtime, and a supply-chain compromise of a published,
// correctly-signed component is precisely how the largest theft in this industry happened.
//
// So the host looks. Parsing a Solana message is byte reading, not cryptography: a header, a
// list of account addresses, a blockhash, and a list of instructions. There are no curves
// here and no dependency on a chain library. The host does not become a wallet; it stops
// signing with its eyes closed.
package signpolicy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Solana approves Solana transaction messages for a key that exists to mint one fixed-supply
// token and nothing else.
//
// The rules are deliberately narrow. This is not a general wallet policy: it is the set of
// things the token extension legitimately needs, and everything else is refused. A policy
// that tried to anticipate every future use would end up permitting the one that drains you.
type Solana struct {
	// Payer is the public key the host will sign with, raw 32 bytes. The message's fee payer
	// must be this key: a message that makes some OTHER account the payer is asking this key
	// to authorise somebody else's transaction.
	Payer []byte
}

// Program IDs, as raw 32-byte addresses. Base58 is a display format; the message carries
// bytes, so these are compared as bytes and this package needs no base58 decoder.
var (
	systemProgram   = mustAddr("11111111111111111111111111111111")
	tokenProgram    = mustAddr("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")
	ataProgram      = mustAddr("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	metadataProgram = mustAddr("metaqbxxUerdq28cj1RbAWkYQm3ybzjb6a8bt518x1s")
)

// SPL Token instruction discriminators (first data byte).
const (
	splMintTo       = 7
	splSetAuthority = 6
)

// SetAuthority authority types.
const (
	authorityMintTokens   = 0
	authorityFreezeAccnt  = 1
	systemTransfer        = 2  // System instruction index (u32 LE) for Transfer
	systemCreateAccount   = 0  // ... and for CreateAccount
	systemTransferWithSed = 11 // ... and for TransferWithSeed
)

// ErrRefused is the class of every policy refusal.
var ErrRefused = errors.New("signing policy refused the payload")

// Approve reports whether the host may sign this payload. A non-nil error means it must not.
//
// The refusals, in order of what they protect:
//
//   - The fee payer must be the key we are signing with. Otherwise the extension is having
//     this key co-sign a transaction whose shape somebody else chose.
//   - Only the four programs a token mint touches may be invoked. Anything else is a use of
//     the key that this key was never granted for.
//   - No System::Transfer, ever. The key pays rent and fees; it does not send SOL to an
//     address of the extension's choosing. This alone caps a total compromise of the
//     extension at the fee balance rather than at "whatever the key can authorise".
//   - No SPL Token instruction except minting and revoking. No transfers, no burns, no
//     freezing, no delegation, no closing accounts.
//   - If the message mints tokens, it must revoke BOTH the mint and the freeze authority in
//     the same message. This is the safety invariant of the token engine, enforced here, on
//     the host, where a compromised extension cannot reach it. The extension proposes; the
//     host disposes. An extension that has been replaced wholesale still cannot obtain a
//     signature over a token it could later inflate or freeze.
func (p Solana) Approve(payload []byte) error {
	msg, err := parseMessage(payload)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRefused, err)
	}

	if len(msg.accounts) == 0 {
		return fmt.Errorf("%w: the message names no accounts", ErrRefused)
	}
	// Account 0 of a Solana message is the fee payer, by definition.
	if !bytes.Equal(msg.accounts[0], p.Payer) {
		return fmt.Errorf("%w: the fee payer is not the granted key, so this key is being asked to authorise somebody else's transaction", ErrRefused)
	}

	var mints, revokedMint, revokedFreeze int
	for i, ix := range msg.instructions {
		prog, err := msg.programOf(ix)
		if err != nil {
			return fmt.Errorf("%w: instruction %d: %w", ErrRefused, i, err)
		}
		switch {
		case bytes.Equal(prog, systemProgram):
			if err := approveSystem(ix.data); err != nil {
				return fmt.Errorf("%w: instruction %d: %w", ErrRefused, i, err)
			}
		case bytes.Equal(prog, tokenProgram):
			kind, err := approveToken(ix.data)
			if err != nil {
				return fmt.Errorf("%w: instruction %d: %w", ErrRefused, i, err)
			}
			switch kind {
			case tokenKindMintTo:
				mints++
			case tokenKindRevokeMint:
				revokedMint++
			case tokenKindRevokeFreeze:
				revokedFreeze++
			case tokenKindOther:
			}
		case bytes.Equal(prog, ataProgram), bytes.Equal(prog, metadataProgram):
			// Creating a token account and writing metadata carry no authority over funds.
		default:
			return fmt.Errorf("%w: instruction %d invokes a program this key is not granted for", ErrRefused, i)
		}
	}

	// The invariant. A message that mints supply must, in the same message, make that supply
	// permanently fixed and permanently unfreezable. Because a Solana transaction is atomic,
	// a signature over such a message cannot be used to obtain the mint without the revokes.
	if mints > 0 {
		// Stated as "at least one", not "not zero": the guard has to hold for every count
		// the loop above could produce, not just the one it happens to produce today.
		if revokedMint < 1 {
			return fmt.Errorf("%w: the message mints tokens but does not revoke the mint authority in the same transaction, so the supply could be inflated afterwards", ErrRefused)
		}
		if revokedFreeze < 1 {
			return fmt.Errorf("%w: the message mints tokens but does not revoke the freeze authority in the same transaction, so holders could be frozen out of selling", ErrRefused)
		}
	}
	return nil
}

func approveSystem(data []byte) error {
	if len(data) < 4 {
		return errors.New("a System instruction with no discriminator")
	}
	switch binary.LittleEndian.Uint32(data[:4]) {
	case systemCreateAccount:
		// Paying rent to bring an account into existence. It moves lamports only to the
		// account being created, which the transaction itself names.
		return nil
	case systemTransfer, systemTransferWithSed:
		return errors.New("System::Transfer: this key pays fees and rent, it does not send SOL to an address the extension chooses")
	default:
		return errors.New("a System instruction this key is not granted for")
	}
}

type tokenKind int

const (
	tokenKindOther tokenKind = iota
	tokenKindMintTo
	tokenKindRevokeMint
	tokenKindRevokeFreeze
)

func approveToken(data []byte) (tokenKind, error) {
	if len(data) == 0 {
		return tokenKindOther, errors.New("an empty SPL Token instruction")
	}
	switch data[0] {
	case splMintTo:
		return tokenKindMintTo, nil
	case splSetAuthority:
		// SetAuthority: [discriminator, authority_type, COption<Pubkey> new_authority].
		// A COption of 0 is None, which is the revoke. Setting an authority to a NEW key is
		// not a revoke and is refused: it would hand the mint to somebody else.
		if len(data) < 3 {
			return tokenKindOther, errors.New("a malformed SetAuthority")
		}
		if data[2] != 0 {
			return tokenKindOther, errors.New("SetAuthority to a new key: this key may revoke an authority, not transfer one")
		}
		switch data[1] {
		case authorityMintTokens:
			return tokenKindRevokeMint, nil
		case authorityFreezeAccnt:
			return tokenKindRevokeFreeze, nil
		default:
			return tokenKindOther, errors.New("SetAuthority over an authority this key is not granted for")
		}
	default:
		return tokenKindOther, errors.New("an SPL Token instruction this key is not granted for (only minting and revoking are)")
	}
}
