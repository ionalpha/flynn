//go:build token

package token

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/ionalpha/flynn/clock"
)

// These tests drive the engine against a fake ledger to prove the mint lifecycle leaves a
// SAFE (non-inflatable) result on its failure paths, not just its happy path. A
// transaction's fate is decided by its signature reaching a terminal on-chain state
// (confirmed, or blockhash-expired past lastValidBlockHeight), never by a fixed timeout, so
// the fake models signature confirmation and block height rather than wall-clock waits.

// setAuthorityDiscriminator is the SPL Token instruction index for SetAuthority; the safety
// revoke is the only instruction the lifecycle emits with it.
const setAuthorityDiscriminator = 6

// fakeRPC is a controllable RPCClient. It respects context cancellation the way a real
// client does, so a detached cleanup context is observably different from a canceled one.
type fakeRPC struct {
	confirm         bool               // GetSignatureStatuses reports the signature confirmed
	unconfirmRevoke bool               // once a revoke is submitted its confirmation never arrives
	lastValid       uint64             // GetLatestBlockhash reports this last-valid block height
	blockHeight     uint64             // GetBlockHeight reports this height (> lastValid means expired)
	cancelOnSend    int                // 1-based send index at which to cancel ctx (outcome unknown)
	cancel          context.CancelFunc // called when cancelOnSend fires
	failSendAt      int                // 1-based send index whose SendTransaction errors
	accountInfoErr  bool               // GetAccountInfo returns a transient error
	accountOwner    solana.PublicKey   // owner GetAccountInfo reports; zero = SPL Token program
	mintData        []byte             // account bytes GetAccountInfo returns; nil = zeroed placeholder
	revokeExpireFor int                // the first N revoke submissions expire (never confirm)
	sigStatusErr     bool // GetSignatureStatuses returns a transient error (status unreadable)
	confirmedOnly    bool // landed signatures report "confirmed" but never reach "finalized"
	cancelAfterStatus int // cancel the context after this many status polls (0 disables)
	sendCount        int
	revokeSends     int
	statusCalls     int
	revokeSubmitted bool // a SetAuthority (revoke) transaction reached SendTransaction
}

func (f *fakeRPC) GetLatestBlockhash(ctx context.Context, _ rpc.CommitmentType) (*rpc.GetLatestBlockhashResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &rpc.GetLatestBlockhashResult{Value: &rpc.LatestBlockhashResult{Blockhash: solana.Hash{1}, LastValidBlockHeight: f.lastValid}}, nil
}

func (f *fakeRPC) GetBlockHeight(_ context.Context, _ rpc.CommitmentType) (uint64, error) {
	return f.blockHeight, nil
}

func (f *fakeRPC) SendTransaction(ctx context.Context, tx *solana.Transaction) (solana.Signature, error) {
	if err := ctx.Err(); err != nil {
		return solana.Signature{}, err
	}
	f.sendCount++
	if isRevoke(tx) {
		f.revokeSends++
		f.revokeSubmitted = true
	}
	if f.cancelOnSend != 0 && f.sendCount == f.cancelOnSend {
		if f.cancel != nil {
			f.cancel()
		}
		return solana.Signature{}, context.Canceled
	}
	if f.failSendAt != 0 && f.sendCount == f.failSendAt {
		// A transport-level failure the moment the tx may have reached the node: the
		// signed transaction can still land even though SendTransaction reports an error.
		return solana.Signature{}, errors.New("rpc: connection reset by peer")
	}
	return solana.Signature{2}, nil
}

func (f *fakeRPC) GetSignatureStatuses(_ context.Context, _ bool, _ ...solana.Signature) (*rpc.GetSignatureStatusesResult, error) {
	f.statusCalls++
	if f.cancelAfterStatus > 0 && f.statusCalls == f.cancelAfterStatus && f.cancel != nil {
		f.cancel() // end the wait after this poll so the test terminates deterministically
	}
	if f.sigStatusErr {
		if f.statusCalls == 1 && f.cancel != nil {
			f.cancel() // end the wait after this errored poll so the test terminates
		}
		return nil, errors.New("rpc: 429 too many requests")
	}
	unconfirmed := &rpc.GetSignatureStatusesResult{Value: []*rpc.SignatureStatusesResult{nil}}
	switch {
	case !f.confirm:
		return unconfirmed, nil
	case f.unconfirmRevoke && f.revokeSubmitted:
		// The revoke was submitted but its confirmation never arrives (it may still land).
		return unconfirmed, nil
	case f.revokeExpireFor > 0 && f.revokeSends > 0 && f.revokeSends <= f.revokeExpireFor:
		// The current revoke attempt expires; a later retry (higher revokeSends) confirms.
		return unconfirmed, nil
	}
	// Landed signatures finalize by default (satisfying both confirmed and finalized
	// requirements); confirmedOnly holds them at confirmed to model a not-yet-final slot.
	status := rpc.ConfirmationStatusFinalized
	if f.confirmedOnly {
		status = rpc.ConfirmationStatusConfirmed
	}
	return &rpc.GetSignatureStatusesResult{Value: []*rpc.SignatureStatusesResult{{ConfirmationStatus: status}}}, nil
}

func (f *fakeRPC) GetAccountInfoWithOpts(_ context.Context, _ solana.PublicKey, _ *rpc.GetAccountInfoOpts) (*rpc.GetAccountInfoResult, error) {
	if f.accountInfoErr {
		return nil, errors.New("rpc: 429 too many requests")
	}
	data := f.mintData
	if data == nil {
		data = make([]byte, mintAccountSize)
	}
	owner := f.accountOwner
	if owner.IsZero() {
		owner = token.ProgramID
	}
	return &rpc.GetAccountInfoResult{Value: &rpc.Account{Owner: owner, Data: rpc.DataBytesOrJSONFromBytes(data)}}, nil
}

func (f *fakeRPC) GetMinimumBalanceForRentExemption(_ context.Context, _ uint64, _ rpc.CommitmentType) (uint64, error) {
	return 1_000_000, nil
}

// revokedMintBytes builds the 82-byte SPL Mint account layout for an initialized mint with
// the mint authority revoked (COption::None), no freeze authority, and the given supply and
// decimals. Layout: mint_authority COption<Pubkey> (4+32), supply u64 (8), decimals u8 (1),
// is_initialized bool (1), freeze_authority COption<Pubkey> (4+32).
func revokedMintBytes(supply uint64, decimals uint8) []byte {
	b := make([]byte, mintAccountSize)
	// bytes[0:4] = 0 -> mint authority COption::None (bytes[4:36] stay zero)
	binary.LittleEndian.PutUint64(b[36:44], supply)
	b[44] = decimals
	b[45] = 1 // is_initialized
	// bytes[46:50] = 0 -> freeze authority COption::None
	return b
}

// isRevoke reports whether tx carries an SPL Token SetAuthority instruction.
func isRevoke(tx *solana.Transaction) bool {
	for _, ci := range tx.Message.Instructions {
		if int(ci.ProgramIDIndex) >= len(tx.Message.AccountKeys) {
			continue
		}
		prog := tx.Message.AccountKeys[ci.ProgramIDIndex]
		if prog.Equals(token.ProgramID) && len(ci.Data) > 0 && ci.Data[0] == setAuthorityDiscriminator {
			return true
		}
	}
	return false
}

// firingClock is a Timing whose timers fire immediately, so the confirm/wait loops do not
// sleep for real. The DECISION to stop still comes from chain state (confirmed signature or
// passed block height), so firing immediately only removes real delay.
type firingClock struct{}

func (firingClock) Now() time.Time { return time.Unix(0, 0).UTC() }

func (firingClock) NewTimer(time.Duration) clock.Timer {
	ch := make(chan time.Time, 1)
	ch <- time.Unix(0, 0).UTC()
	return firedTimer{ch}
}

func (firingClock) After(d time.Duration) <-chan time.Time { return firingClock{}.NewTimer(d).C() }

type firedTimer struct{ ch chan time.Time }

func (t firedTimer) C() <-chan time.Time    { return t.ch }
func (firedTimer) Stop() bool               { return true }
func (firedTimer) Reset(time.Duration) bool { return true }

// testPayer returns a deterministic in-process signer (fixed seed, no randomness).
func testPayer() KeySigner {
	return KeySigner{Key: solana.PrivateKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))}
}

func newTestEngine(f *fakeRPC) *Engine {
	e := NewEngine(f, testPayer())
	e.clk = firingClock{}
	return e
}

// scaled returns whole * 10^decimals, matching what the engine mints.
func scaled(whole uint64, decimals uint8) uint64 {
	amount := whole
	for range decimals {
		amount *= 10
	}
	return amount
}

func safeSpec() MintSpec {
	return MintSpec{Name: "Flynn", Symbol: "FLYNN", MetadataURI: "https://example.com/token.json", Decimals: 9, Supply: 1}
}

// TestMintHappyPathSucceeds guards the success path: every step confirms, and the on-chain
// mint verifies as revoked, freeze-free, and holding the whole supply.
func TestMintHappyPathSucceeds(t *testing.T) {
	s := safeSpec()
	f := &fakeRPC{confirm: true, lastValid: 100, mintData: revokedMintBytes(scaled(s.Supply, s.Decimals), s.Decimals)}
	eng := newTestEngine(f)

	mint, _, err := eng.Mint(context.Background(), s)
	if err != nil {
		t.Fatalf("happy-path mint failed: %v", err)
	}
	if mint.IsZero() {
		t.Fatal("expected a mint address on success")
	}
}

// TestCreateMintReturnsZeroWhenExpired proves an expired create (its blockhash passed
// without landing) yields the zero address: nothing landed on-chain, so there is nothing to
// clean up and no phantom mint is reported.
func TestCreateMintReturnsZeroWhenExpired(t *testing.T) {
	f := &fakeRPC{confirm: false, lastValid: 100, blockHeight: 200}
	eng := newTestEngine(f)

	mint, err := eng.CreateMint(context.Background(), 9)
	if err == nil {
		t.Fatal("expected an expiry error")
	}
	if !mint.IsZero() {
		t.Fatalf("expired create returned a non-zero address %s; nothing landed, so nothing exists to clean up", mint)
	}
}

// TestCreateMintReturnsAddressWhenUnresolved proves an unresolved create (submitted, outcome
// unknown because the context ended before it confirmed or expired) hands back the mint
// address so the caller can revoke on a best-effort basis rather than strand a possible mint.
func TestCreateMintReturnsAddressWhenUnresolved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// blockHeight <= lastValid so the tx has NOT expired; cancel at send so confirmOrExpire
	// ends with an unknown outcome rather than a definitive expiry.
	f := &fakeRPC{confirm: false, lastValid: 100, blockHeight: 50, cancelOnSend: 1, cancel: cancel}
	eng := newTestEngine(f)

	mint, err := eng.CreateMint(ctx, 9)
	if err == nil {
		t.Fatal("expected an unresolved-create error")
	}
	if mint.IsZero() {
		t.Fatalf("unresolved create returned a zero address; a mint that may exist cannot be revoked (err=%v)", err)
	}
}

// TestSubmitErrorDoesNotAbortLandedTx proves a SendTransaction error does not abort a
// transaction that still lands: the fate is decided by watching the signature, not by the
// submit call, so a lost submit response for a tx that confirms is a success.
func TestSubmitErrorDoesNotAbortLandedTx(t *testing.T) {
	f := &fakeRPC{confirm: true, lastValid: 100, failSendAt: 1}
	eng := newTestEngine(f)

	mint, err := eng.CreateMint(context.Background(), 9)
	if err != nil {
		t.Fatalf("a create that landed despite a submit error was reported as failed: %v", err)
	}
	if mint.IsZero() {
		t.Fatal("expected the mint address for a landed create")
	}
}

// TestMintRevokesAfterCancellationDuringFinalize proves the safety revoke is still submitted
// when the caller's context is canceled during finalize. The cleanup runs on a detached
// context, so a caller cancellation cannot also prevent the revoke.
func TestMintRevokesAfterCancellationDuringFinalize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// confirm=true so the create (send #1) lands; send #2 is metadata, where the caller
	// context is canceled, forcing the cleanup path.
	f := &fakeRPC{confirm: true, lastValid: 100, cancelOnSend: 2, cancel: cancel}
	eng := newTestEngine(f)

	_, _, err := eng.Mint(ctx, safeSpec())
	if err == nil {
		t.Fatal("expected an error from the canceled finalize")
	}
	if !f.revokeSubmitted {
		t.Fatal("safety revoke was never submitted after the finalize failure; the created mint is left inflatable")
	}
}

// TestMintSucceedsWhenFinalRevokeLandsUnconfirmed proves a token whose lifecycle revoke was
// submitted but never confirmed (yet landed on-chain) is reported as the safe, complete
// token it is. finalize returns a confirmation error, but on-chain the authority is revoked
// and the whole supply is present, so Mint trusts the verified state and succeeds.
func TestMintSucceedsWhenFinalRevokeLandsUnconfirmed(t *testing.T) {
	s := safeSpec()
	// confirm=true so create/metadata/supply confirm; unconfirmRevoke makes the final revoke
	// submitted-but-unconfirmed, and blockHeight past lastValid makes that revoke read as
	// expired; the account already reflects a revoked, fully minted mint.
	f := &fakeRPC{
		confirm: true, unconfirmRevoke: true, lastValid: 100, blockHeight: 200,
		mintData: revokedMintBytes(scaled(s.Supply, s.Decimals), s.Decimals),
	}
	eng := newTestEngine(f)

	mint, _, err := eng.Mint(context.Background(), s)
	if err != nil {
		t.Fatalf("a safe, fully minted token was reported as failed: %v", err)
	}
	if mint.IsZero() {
		t.Fatal("expected the mint address on success")
	}
}

// TestAbortRetriesExpiredRevoke proves the cleanup revoke is retried with a fresh blockhash
// when it expires without landing, rather than mistaking an expired revoke for a completed
// one. The first revoke expires; the second confirms.
func TestAbortRetriesExpiredRevoke(t *testing.T) {
	f := &fakeRPC{confirm: true, lastValid: 100, blockHeight: 200, revokeExpireFor: 1}
	eng := newTestEngine(f)

	_, _, err := eng.abortMint(context.Background(), solana.PublicKey{1}, nil, errors.New("finalize failed"))
	if err == nil || !strings.Contains(err.Error(), "authority revoked so supply is fixed") {
		t.Fatalf("expected a successful revoke after retry, got: %v", err)
	}
	if f.revokeSends != 2 {
		t.Fatalf("expected the expired revoke to be retried once (2 sends), got %d", f.revokeSends)
	}
}

// TestAbortReportsUnresolvedWhenRevokeNeverLands proves cleanup reports the mint as possibly
// mintable (never as safe) when every revoke attempt expires: the honest, conservative
// outcome when the authority cannot be confirmed revoked.
func TestAbortReportsUnresolvedWhenRevokeNeverLands(t *testing.T) {
	f := &fakeRPC{confirm: false, lastValid: 100, blockHeight: 200}
	eng := newTestEngine(f)

	_, _, err := eng.abortMint(context.Background(), solana.PublicKey{1}, nil, errors.New("finalize failed"))
	if err == nil || !strings.Contains(err.Error(), "may remain mintable") {
		t.Fatalf("expected an unresolved/possibly-mintable report, got: %v", err)
	}
	if f.revokeSends != revokeAttempts {
		t.Fatalf("expected %d revoke attempts before giving up, got %d", revokeAttempts, f.revokeSends)
	}
}

// TestConfirmOrExpireDoesNotExpireOnUnreadableStatus proves an unreadable signature status
// is never treated as proof a transaction expired: a transient status error while the block
// height passes lastValidBlockHeight leaves the outcome UNKNOWN (the tx may have landed), not
// expired, so a landed mint is never silently dropped from cleanup.
func TestConfirmOrExpireDoesNotExpireOnUnreadableStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Every status read errors and the blockhash is already past its last valid height; the
	// fake cancels the context after the first errored poll so the wait ends as unknown.
	f := &fakeRPC{sigStatusErr: true, lastValid: 100, blockHeight: 200, cancel: cancel}
	eng := newTestEngine(f)

	err := eng.confirmOrExpire(ctx, solana.Signature{9}, f.lastValid, rpc.CommitmentConfirmed)
	if errors.Is(err, errTxExpired) {
		t.Fatal("an unreadable signature status was treated as proof the transaction expired; a landed tx would be dropped from cleanup")
	}
}

// TestConfirmOrExpireRequiresFinalized proves a merely confirmed signature is NOT accepted
// as terminal when finalized commitment is required, so the irreversible revoke is never
// reported done off a slot that could still be forked out.
func TestConfirmOrExpireRequiresFinalized(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// The signature reaches "confirmed" but never "finalized"; the fake cancels the context
	// after the first poll so the wait ends without a spurious success.
	f := &fakeRPC{confirm: true, confirmedOnly: true, lastValid: 100, cancel: cancel}
	f.cancelAfterStatus = 1
	eng := newTestEngine(f)

	err := eng.confirmOrExpire(ctx, solana.Signature{9}, f.lastValid, rpc.CommitmentFinalized)
	if err == nil {
		t.Fatal("a confirmed-but-unfinalized signature was accepted as finalized; a forked-out revoke would be reported as permanent")
	}
}

// TestConfirmOrExpireAcceptsConfirmedWhenAllowed proves the same confirmed signature IS
// accepted when only confirmed commitment is required, so non-critical steps are not forced
// to wait for finality.
func TestConfirmOrExpireAcceptsConfirmedWhenAllowed(t *testing.T) {
	f := &fakeRPC{confirm: true, confirmedOnly: true, lastValid: 100}
	eng := newTestEngine(f)

	if err := eng.confirmOrExpire(context.Background(), solana.Signature{9}, f.lastValid, rpc.CommitmentConfirmed); err != nil {
		t.Fatalf("a confirmed signature was not accepted at confirmed commitment: %v", err)
	}
}

// TestCreateMetadataRequiresFinalized proves the metadata attach is not accepted at merely
// confirmed commitment. The mint's success report asserts the name/symbol but the final
// verify never re-reads the metadata, so a confirmed-but-forkable metadata slot could be
// dropped while the token is reported safe; the attach must finalize.
func TestCreateMetadataRequiresFinalized(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeRPC{confirm: true, confirmedOnly: true, lastValid: 100, cancel: cancel}
	f.cancelAfterStatus = 1
	eng := newTestEngine(f)

	if err := eng.CreateMetadata(ctx, solana.PublicKey{3}, "Name", "SYM", "https://example.com/y.json"); err == nil {
		t.Fatal("CreateMetadata accepted a merely-confirmed metadata tx; a forked-out slot would drop the metadata while the mint is reported safe")
	}
}

// TestVerifyFlagsToken2022AsUnsafe proves a Token-2022 mint (which can carry transfer
// hooks/fees/permanent delegate) is reported as its own UNSAFE class, not decoded as a plain
// mint and not returned as a bare "wrong owner" error a caller might read as "couldn't check".
func TestVerifyFlagsToken2022AsUnsafe(t *testing.T) {
	f := &fakeRPC{accountOwner: solana.MustPublicKeyFromBase58("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")}
	eng := newTestEngine(f)

	if _, err := eng.Verify(context.Background(), solana.PublicKey{7}); !errors.Is(err, errToken2022Mint) {
		t.Fatalf("expected a Token-2022 mint to be flagged unsafe, got: %v", err)
	}

	out, terr := verifyTool{eng}.Invoke(context.Background(), []byte(`{"mint":"So11111111111111111111111111111111111111112"}`))
	if terr != nil {
		t.Fatalf("token_verify should render a verdict, not an error: %v", terr)
	}
	if !strings.Contains(out, "UNSAFE") || !strings.Contains(out, "Token-2022") {
		t.Fatalf("token_verify did not report the Token-2022 mint as UNSAFE: %q", out)
	}
}
