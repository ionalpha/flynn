//go:build token

package token

import (
	"context"
	"errors"
	"fmt"

	solana "github.com/gagliardetto/solana-go"

	"github.com/ionalpha/flynn/extensions/token/safety"
)

// MintSpec describes a token to mint. Only safe options are exposed: the scam levers
// (freeze authority, transfer hook, permanent delegate, transfer fee) are not
// configurable here, so the engine cannot produce them, and the safety policy is
// enforced before anything is signed as a second, explicit guard.
type MintSpec struct {
	Name        string
	Symbol      string
	MetadataURI string
	Decimals    uint8
	Supply      uint64 // whole tokens; scaled by decimals when minted
}

// Mint runs the full guarded lifecycle and returns the mint address plus any
// warn-level disclosures the safety policy attached (a caller must surface these; a
// blocking shape is refused, not returned). It builds the token's final intended
// shape, refuses via the safety policy if that shape is scam-like, then runs create
// -> metadata -> supply -> revoke mint authority, and finally verifies on-chain that
// the result matches the safe shape. A scam-shaped request never reaches signing.
func (e *Engine) Mint(ctx context.Context, s MintSpec) (solana.PublicKey, []safety.Violation, error) {
	// Validate deterministic inputs BEFORE any on-chain action, so an invalid request
	// (a supply that overflows when scaled, or metadata that exceeds the Metaplex
	// limits) is rejected up front and never leaves a partially-created mint with
	// authority still retained.
	if err := validateMetadata(s.Name, s.Symbol, s.MetadataURI); err != nil {
		return solana.PublicKey{}, nil, err
	}
	if _, err := scaledAmount(s.Supply, s.Decimals); err != nil {
		return solana.PublicKey{}, nil, err
	}

	// The final shape this engine produces: fixed supply (mint authority revoked), no
	// freeze/hook/delegate/fee, no yield or profit claim. The whole supply is minted to
	// the payer (the treasury), so the plan records full creator retention; the plan
	// also carries the requested identity so an impersonating name/symbol is refused.
	plan := safety.TokenPlan{
		Chain:            "solana",
		Op:               "mint",
		CreatorSupplyPct: 100,
		Impersonates:     safety.ImpersonationTarget(s.Name, s.Symbol),
	}
	if err := safety.Guard(plan); err != nil {
		return solana.PublicKey{}, nil, err
	}
	disclosures := safety.Evaluate(plan) // warn-level only; Guard already refused any blocking shape

	mint, err := e.CreateMint(ctx, s.Decimals)
	if err != nil {
		// CreateMint returns a non-zero address when it submitted the create
		// transaction but could not confirm it, so the mint may already exist
		// on-chain. abortMint revokes on a best-effort basis (and does nothing for a
		// zero address, meaning nothing was ever submitted).
		return e.abortMint(ctx, mint, disclosures, err)
	}

	// The mint now exists with the payer as its mint authority. finalize runs metadata
	// -> supply -> revoke; its final step (the revoke) can be submitted but not confirmed
	// and still land, so a finalize error does NOT by itself mean the token is unsafe or
	// incomplete. The authority and supply ON-CHAIN are the source of truth, so verify
	// the real state and judge by that rather than by the last RPC result: otherwise a
	// safe, fully minted token whose revoke merely lost its confirmation is reported as a
	// failed, unsafe mint.
	ferr := e.finalizeMint(ctx, mint, s)

	st, verr := e.Verify(context.WithoutCancel(ctx), mint)
	if verr != nil {
		// The state cannot be read, so safety cannot be proven: revoke the authority
		// best-effort and surface both causes.
		return e.abortMint(ctx, mint, disclosures, errors.Join(ferr, verr))
	}
	if st.Freezable() {
		return mint, disclosures, fmt.Errorf("post-mint verify failed: a freeze authority is present on %s", mint)
	}
	if !st.SupplyFixed() {
		// The authority is still live, so the mint could be inflated: revoke it. On the
		// happy path (ferr == nil) this means a revoke that reported success did not take,
		// so re-revoke rather than trust the earlier result.
		cause := ferr
		if cause == nil {
			cause = fmt.Errorf("post-mint verify: mint authority is NOT revoked on %s", mint)
		}
		return e.abortMint(ctx, mint, disclosures, cause)
	}
	// Authority revoked and no freeze authority: the mint is safe. If it also holds the
	// whole requested supply the token is complete, even when finalize reported a late
	// error on its already-landed revoke.
	if expected, aerr := scaledAmount(s.Supply, s.Decimals); aerr == nil && st.Supply != expected {
		incomplete := fmt.Sprintf("mint %s is safe (authority revoked, no freeze) but holds supply %d, not the requested %d", mint, st.Supply, expected)
		if ferr != nil {
			return mint, disclosures, fmt.Errorf("%s: %w", incomplete, ferr)
		}
		return mint, disclosures, errors.New(incomplete)
	}
	return mint, disclosures, nil
}

// abortMint revokes the mint authority after a mid-lifecycle failure so a created but
// unfinished mint can never be inflated, then returns the wrapped cause. The revoke
// runs on a context detached from the caller's cancellation and deadline (via
// context.WithoutCancel), so a timeout or cancellation that caused the original failure
// cannot also prevent the safety revoke from being submitted. A zero mint address means
// creation never submitted a transaction, so there is nothing on-chain to revoke.
func (e *Engine) abortMint(ctx context.Context, mint solana.PublicKey, disclosures []safety.Violation, cause error) (solana.PublicKey, []safety.Violation, error) {
	if mint.IsZero() {
		return solana.PublicKey{}, disclosures, cause
	}
	// Detach from the caller's cancellation/deadline so the timeout that caused the
	// original failure cannot also prevent the safety revoke from being submitted.
	cleanupCtx := context.WithoutCancel(ctx)
	// A create that was submitted but not confirmed can still be in flight, so the mint
	// account may not be visible yet. Revoking now would fail preflight as
	// account-not-found and then the create could land afterwards, leaving the mint
	// authority retained. Wait for the account to become visible before revoking.
	found, sawError := e.awaitAccount(cleanupCtx, mint)
	if !found && !sawError {
		// Every poll cleanly reported the account absent: the create did not land (its
		// blockhash has expired), so there is nothing on-chain to revoke.
		return mint, disclosures, fmt.Errorf("mint %s aborted after creation; its account never appeared so the create did not land: %w", mint, cause)
	}
	// The account is visible, or its existence could not be determined (transient RPC
	// errors or a timed-out wait). Attempt the revoke either way: skipping it on
	// uncertainty would strand a live mint authority on a mint that actually landed.
	if rerr := e.RevokeMintAuthority(cleanupCtx, mint); rerr != nil {
		// An earlier unconfirmed revoke may since have landed, clearing the authority and
		// making this retry fail; trust the on-chain state over the retry error so a mint
		// that is actually fixed is never reported as possibly mintable.
		if st, verr := e.Verify(cleanupCtx, mint); verr == nil && st.SupplyFixed() {
			return mint, disclosures, fmt.Errorf("mint %s aborted after creation; mint authority already revoked so supply is fixed: %w", mint, cause)
		}
		return mint, disclosures, fmt.Errorf("mint %s aborted after creation and the safety revoke also failed (it may remain mintable): %w", mint, errors.Join(cause, rerr))
	}
	return mint, disclosures, fmt.Errorf("mint %s aborted after creation; mint authority revoked so supply is fixed: %w", mint, cause)
}

// finalizeMint attaches metadata, mints the whole supply, and revokes the mint
// authority. It is the part of the lifecycle that runs after the mint exists; the
// caller reacts to any error by revoking the mint authority so the mint is never left
// mintable.
func (e *Engine) finalizeMint(ctx context.Context, mint solana.PublicKey, s MintSpec) error {
	if err := e.CreateMetadata(ctx, mint, s.Name, s.Symbol, s.MetadataURI); err != nil {
		return err
	}
	if err := e.MintSupply(ctx, mint, s.Supply, s.Decimals); err != nil {
		return err
	}
	return e.RevokeMintAuthority(ctx, mint)
}

// Metaplex Token Metadata field byte limits.
const (
	maxNameLen   = 32
	maxSymbolLen = 10
	maxURILen    = 200
)

// validateMetadata rejects identity fields that exceed the Metaplex limits (or an
// empty name/symbol) before any on-chain action, so a too-long field never fails the
// metadata step after the mint already exists.
func validateMetadata(name, symbol, uri string) error {
	switch {
	case len(name) == 0 || len(name) > maxNameLen:
		return fmt.Errorf("name must be 1..%d bytes, got %d", maxNameLen, len(name))
	case len(symbol) == 0 || len(symbol) > maxSymbolLen:
		return fmt.Errorf("symbol must be 1..%d bytes, got %d", maxSymbolLen, len(symbol))
	case len(uri) > maxURILen:
		return fmt.Errorf("uri must be at most %d bytes, got %d", maxURILen, len(uri))
	}
	return nil
}
