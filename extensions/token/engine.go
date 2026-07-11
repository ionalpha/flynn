//go:build token

// Package token is an optional, capability-gated Solana token engine: it creates a
// fixed-supply SPL token, attaches and edits Metaplex metadata, revokes the mint
// authority, and verifies the result, entirely in-process. It is not part of the
// default build; a host mounts it behind the token capability.
//
// Every mutating step is a method that returns an error, never a process exit, so
// the engine composes under the governed dispatch waist. The engine performs the
// mechanics only; the safety policy that forbids scam-shaped tokens wraps it
// separately and is what a host actually grants.
package token

import (
	"context"
	"fmt"
	"math"
	"time"

	bin "github.com/gagliardetto/binary"
	solana "github.com/gagliardetto/solana-go"
	ata "github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/ionalpha/flynn/clock"
)

// mintAccountSize is the byte length of an SPL Mint account.
const mintAccountSize = 82

// metadataProgram is the Metaplex Token Metadata program.
var metadataProgram = solana.MustPublicKeyFromBase58("metaqbxxUerdq28cj1RbAWkYQm3ybzjb6a8bt518x1s")

var sysvarInstructions = solana.MustPublicKeyFromBase58("Sysvar1nstructions1111111111111111111111111")

// RPCClient is the subset of the Solana RPC the engine uses, named as an interface
// so a test can drive the engine against a fake ledger.
type RPCClient interface {
	GetLatestBlockhash(ctx context.Context, commitment rpc.CommitmentType) (*rpc.GetLatestBlockhashResult, error)
	SendTransaction(ctx context.Context, tx *solana.Transaction) (solana.Signature, error)
	GetSignatureStatuses(ctx context.Context, searchTransactionHistory bool, sigs ...solana.Signature) (*rpc.GetSignatureStatusesResult, error)
	GetAccountInfoWithOpts(ctx context.Context, account solana.PublicKey, opts *rpc.GetAccountInfoOpts) (*rpc.GetAccountInfoResult, error)
	GetMinimumBalanceForRentExemption(ctx context.Context, dataSize uint64, commitment rpc.CommitmentType) (uint64, error)
}

// Signer authorizes transactions by signing a serialized message. Every method is
// exported, so a caller in another package can supply a vault- or hardware-backed
// signer without touching the engine.
type Signer interface {
	PublicKey() solana.PublicKey
	Sign(message []byte) (solana.Signature, error)
}

// KeySigner is an in-process Signer backed by a private key (devnet/tests). A real
// deployment supplies a hardware- or multisig-backed Signer instead.
type KeySigner struct{ Key solana.PrivateKey }

// PublicKey returns the signer's public key.
func (k KeySigner) PublicKey() solana.PublicKey { return k.Key.PublicKey() }

// Sign signs message with the backing private key.
func (k KeySigner) Sign(message []byte) (solana.Signature, error) { return k.Key.Sign(message) }

// Engine runs token operations against one cluster as one payer/authority.
type Engine struct {
	rpc   RPCClient
	payer Signer
	clk   clock.Timing
}

// NewEngine builds an engine over an RPC client and a payer/authority signer.
func NewEngine(client RPCClient, payer Signer) *Engine {
	return &Engine{rpc: client, payer: payer, clk: clock.System{}}
}

// MintState is the observable, verifiable state of a mint.
type MintState struct {
	Mint            solana.PublicKey
	Decimals        uint8
	Supply          uint64
	MintAuthority   *solana.PublicKey // nil means revoked (supply fixed)
	FreezeAuthority *solana.PublicKey // nil means no freeze authority
}

// SupplyFixed reports whether new tokens can never be minted.
func (m MintState) SupplyFixed() bool { return m.MintAuthority == nil }

// Freezable reports whether any account can be frozen.
func (m MintState) Freezable() bool { return m.FreezeAuthority != nil }

// CreateMint creates a fresh mint with the payer as mint authority and NO freeze
// authority, returning its address. Metadata must be attached before the mint
// authority is revoked, so this does not revoke anything.
func (e *Engine) CreateMint(ctx context.Context, decimals uint8) (solana.PublicKey, error) {
	mint := solana.NewWallet()
	rent, err := e.rpc.GetMinimumBalanceForRentExemption(ctx, mintAccountSize, rpc.CommitmentFinalized)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("rent exemption: %w", err)
	}
	create := system.NewCreateAccountInstruction(rent, mintAccountSize, token.ProgramID, e.payer.PublicKey(), mint.PublicKey()).Build()
	initMint, err := token.NewInitializeMint2InstructionBuilder().
		SetDecimals(decimals).
		SetMintAuthority(e.payer.PublicKey()).
		SetMintAccount(mint.PublicKey()).
		ValidateAndBuild()
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("build initialize mint: %w", err)
	}
	sig, err := e.send(ctx, []solana.Instruction{create, initMint}, KeySigner{Key: mint.PrivateKey})
	if err != nil {
		if sig.IsZero() {
			// Failed before the transaction was submitted (blockhash, build, or
			// signing): no account was created, so there is nothing to clean up.
			return solana.PublicKey{}, fmt.Errorf("create mint: %w", err)
		}
		// Submitted but not confirmed: the mint account may exist on-chain. Return its
		// address so the caller can revoke the mint authority on a best-effort basis
		// rather than stranding an inflatable mint it cannot name.
		return mint.PublicKey(), fmt.Errorf("create mint submitted but unconfirmed: %w", err)
	}
	if err := e.waitForAccount(ctx, mint.PublicKey()); err != nil {
		// The create transaction confirmed, so the account exists; only its finalized
		// visibility lagged. Return the address so the caller can still clean up.
		return mint.PublicKey(), fmt.Errorf("await mint visibility: %w", err)
	}
	return mint.PublicKey(), nil
}

// MintSupply creates the payer's associated token account and mints the whole
// supply (scaled by decimals) into it.
func (e *Engine) MintSupply(ctx context.Context, mint solana.PublicKey, whole uint64, decimals uint8) error {
	owner := e.payer.PublicKey()
	dest, _, err := solana.FindAssociatedTokenAddress(owner, mint)
	if err != nil {
		return fmt.Errorf("derive ATA: %w", err)
	}
	createATA, err := ata.NewCreateInstructionBuilder().SetPayer(owner).SetWallet(owner).SetMint(mint).ValidateAndBuild()
	if err != nil {
		return fmt.Errorf("build create ATA: %w", err)
	}
	amount, err := scaledAmount(whole, decimals)
	if err != nil {
		return err
	}
	mintTo, err := token.NewMintToInstructionBuilder().
		SetAmount(amount).SetMintAccount(mint).SetDestinationAccount(dest).SetAuthorityAccount(owner).
		ValidateAndBuild()
	if err != nil {
		return fmt.Errorf("build mint-to: %w", err)
	}
	_, err = e.send(ctx, []solana.Instruction{createATA, mintTo})
	return err
}

// scaledAmount returns whole scaled by 10^decimals, or an error if the result
// overflows uint64. Callers validate this before any on-chain action so an invalid
// supply never leaves a partially-created mint behind.
func scaledAmount(whole uint64, decimals uint8) (uint64, error) {
	amount := whole
	for range decimals {
		if amount > math.MaxUint64/10 {
			return 0, fmt.Errorf("supply %d with %d decimals overflows uint64", whole, decimals)
		}
		amount *= 10
	}
	return amount, nil
}

// RevokeMintAuthority sets the mint authority to None: supply is permanently fixed.
// This is irreversible.
func (e *Engine) RevokeMintAuthority(ctx context.Context, mint solana.PublicKey) error {
	ix, err := token.NewSetAuthorityInstructionBuilder().
		SetAuthorityType(token.AuthorityMintTokens).
		SetSubjectAccount(mint).
		SetAuthorityAccount(e.payer.PublicKey()).
		ValidateAndBuild()
	if err != nil {
		return fmt.Errorf("build set-authority: %w", err)
	}
	_, err = e.send(ctx, []solana.Instruction{ix})
	return err
}

// Verify fetches and decodes the mint, returning its observable state.
func (e *Engine) Verify(ctx context.Context, mint solana.PublicKey) (MintState, error) {
	info, err := e.rpc.GetAccountInfoWithOpts(ctx, mint, &rpc.GetAccountInfoOpts{Commitment: rpc.CommitmentConfirmed})
	if err != nil {
		return MintState{}, fmt.Errorf("fetch mint: %w", err)
	}
	if info == nil || info.Value == nil {
		return MintState{}, fmt.Errorf("mint %s not found", mint)
	}
	// Guard against decoding a non-mint account as a valid mint, which would otherwise
	// report null authorities as a "safe" token. A mint is owned by the SPL Token
	// program AND is exactly mintAccountSize bytes; a token account (165 bytes) or a
	// multisig is a different, larger layout owned by the same program.
	data := info.Value.Data.GetBinary()
	if !info.Value.Owner.Equals(token.ProgramID) || len(data) != mintAccountSize {
		return MintState{}, fmt.Errorf("account %s is not an SPL mint (wrong owner or size)", mint)
	}
	var m token.Mint
	if err := bin.NewBinDecoder(data).Decode(&m); err != nil {
		return MintState{}, fmt.Errorf("decode mint: %w", err)
	}
	if !m.IsInitialized {
		return MintState{}, fmt.Errorf("account %s is not an initialized SPL mint", mint)
	}
	return MintState{
		Mint: mint, Decimals: m.Decimals, Supply: m.Supply,
		MintAuthority: m.MintAuthority, FreezeAuthority: m.FreezeAuthority,
	}, nil
}

// send builds, signs (with the payer plus any extra signers), submits, and confirms
// a transaction, returning its signature. It signs the serialized message through the
// Signer interface, so a hardware- or multisig-backed payer works without exposing a
// private key.
func (e *Engine) send(ctx context.Context, ixs []solana.Instruction, extra ...Signer) (solana.Signature, error) {
	bh, err := e.rpc.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("blockhash: %w", err)
	}
	tx, err := solana.NewTransaction(ixs, bh.Value.Blockhash, solana.TransactionPayer(e.payer.PublicKey()))
	if err != nil {
		return solana.Signature{}, fmt.Errorf("new tx: %w", err)
	}
	signers := append([]Signer{e.payer}, extra...)
	msg, err := tx.Message.MarshalBinary()
	if err != nil {
		return solana.Signature{}, fmt.Errorf("marshal message: %w", err)
	}
	tx.Signatures = make([]solana.Signature, tx.Message.Header.NumRequiredSignatures)
	for i := range tx.Signatures {
		want := tx.Message.AccountKeys[i]
		var s Signer
		for _, cand := range signers {
			if cand.PublicKey().Equals(want) {
				s = cand
				break
			}
		}
		if s == nil {
			return solana.Signature{}, fmt.Errorf("no signer for required account %s", want)
		}
		if tx.Signatures[i], err = s.Sign(msg); err != nil {
			return solana.Signature{}, fmt.Errorf("sign: %w", err)
		}
	}
	// The transaction signature is its fee-payer signature (tx.Signatures[0]), fixed the
	// moment the tx is signed and identical to what SendTransaction returns. Capture it
	// before submitting so a SendTransaction error is reported WITH the real signature:
	// the signed transaction may still have reached the node and can land, so the caller
	// must be able to name and clean up the resulting account. A zero signature is
	// reserved for failures before signing, where nothing can ever land.
	sig := tx.Signatures[0]
	if _, err := e.rpc.SendTransaction(ctx, tx); err != nil {
		return sig, fmt.Errorf("send: %w", err)
	}
	if err := e.confirm(ctx, sig); err != nil {
		// Submission succeeded but confirmation did not: the transaction may still
		// land, so return the real signature alongside the error so a caller can
		// distinguish "submitted, unconfirmed" from "never submitted".
		return sig, err
	}
	return sig, nil
}

// confirm polls until the signature is confirmed or finalized, or the context ends.
func (e *Engine) confirm(ctx context.Context, sig solana.Signature) error {
	for range 30 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.clk.After(2 * time.Second):
		}
		st, err := e.rpc.GetSignatureStatuses(ctx, true, sig)
		if err != nil || len(st.Value) == 0 || st.Value[0] == nil {
			continue
		}
		s := st.Value[0]
		if s.Err != nil {
			return fmt.Errorf("tx %s failed on-chain: %v", sig, s.Err)
		}
		if s.ConfirmationStatus == rpc.ConfirmationStatusConfirmed || s.ConfirmationStatus == rpc.ConfirmationStatusFinalized {
			return nil
		}
	}
	return fmt.Errorf("timed out confirming %s", sig)
}

// awaitAccount polls for an account at finalized commitment. found is true once the
// account is observed with data. sawError is true if any poll failed with an RPC error
// (or the context ended): a false found is then UNKNOWN, not a proof of absence, because
// a transient 429/network error is not evidence the account does not exist. Callers that
// must not skip a safety action on uncertainty use sawError to tell "cleanly absent" from
// "could not determine".
func (e *Engine) awaitAccount(ctx context.Context, pubkey solana.PublicKey) (found, sawError bool) {
	for range 30 {
		select {
		case <-ctx.Done():
			return false, true // cancellation/timeout is not proof of absence
		case <-e.clk.After(2 * time.Second):
		}
		info, err := e.rpc.GetAccountInfoWithOpts(ctx, pubkey, &rpc.GetAccountInfoOpts{Commitment: rpc.CommitmentFinalized})
		switch {
		case err != nil:
			sawError = true
		case info != nil && info.Value != nil && len(info.Value.Data.GetBinary()) > 0:
			return true, sawError
		}
	}
	return false, sawError
}

// waitForAccount blocks until an account exists with non-empty data at finalized
// commitment, so a freshly created account is visible to every node before the next
// instruction reads it (avoids a propagation race on public RPC).
func (e *Engine) waitForAccount(ctx context.Context, pubkey solana.PublicKey) error {
	if found, _ := e.awaitAccount(ctx, pubkey); !found {
		return fmt.Errorf("account %s not visible (finalized) after wait", pubkey)
	}
	return nil
}

func metadataPDA(mint solana.PublicKey) (solana.PublicKey, error) {
	pda, _, err := solana.FindProgramAddress(
		[][]byte{[]byte("metadata"), metadataProgram.Bytes(), mint.Bytes()}, metadataProgram,
	)
	return pda, err
}
