//go:build token

package token

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/ionalpha/flynn/clock"
)

// These tests drive the engine against a fake ledger to prove the mint lifecycle
// leaves a SAFE (non-inflatable) result on its failure paths, not just its happy path.
// They exercise the two cases builds and lint never reach: a create transaction that
// is submitted but never confirmed, and a caller cancellation during finalize.

// setAuthorityDiscriminator is the SPL Token instruction index for SetAuthority; the
// safety revoke is the only instruction the lifecycle emits with it.
const setAuthorityDiscriminator = 6

// fakeRPC is a controllable RPCClient. It respects context cancellation the way a real
// client does, so a detached cleanup context is observably different from a canceled
// one.
type fakeRPC struct {
	confirm         bool               // GetSignatureStatuses reports confirmed
	cancelOnSend    int                // 1-based send index at which to cancel ctx and fail; 0 disables
	cancel          context.CancelFunc // called when cancelOnSend fires
	sendCount       int
	revokeSubmitted bool // a SetAuthority (revoke) transaction reached SendTransaction
}

func (f *fakeRPC) GetLatestBlockhash(ctx context.Context, _ rpc.CommitmentType) (*rpc.GetLatestBlockhashResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &rpc.GetLatestBlockhashResult{Value: &rpc.LatestBlockhashResult{Blockhash: solana.Hash{1}}}, nil
}

func (f *fakeRPC) SendTransaction(ctx context.Context, tx *solana.Transaction) (solana.Signature, error) {
	if err := ctx.Err(); err != nil {
		return solana.Signature{}, err
	}
	f.sendCount++
	if f.cancelOnSend != 0 && f.sendCount == f.cancelOnSend {
		if f.cancel != nil {
			f.cancel()
		}
		return solana.Signature{}, context.Canceled
	}
	if isRevoke(tx) {
		f.revokeSubmitted = true
	}
	return solana.Signature{2}, nil
}

func (f *fakeRPC) GetSignatureStatuses(_ context.Context, _ bool, _ ...solana.Signature) (*rpc.GetSignatureStatusesResult, error) {
	if !f.confirm {
		return &rpc.GetSignatureStatusesResult{Value: []*rpc.SignatureStatusesResult{nil}}, nil
	}
	return &rpc.GetSignatureStatusesResult{Value: []*rpc.SignatureStatusesResult{{ConfirmationStatus: rpc.ConfirmationStatusConfirmed}}}, nil
}

func (f *fakeRPC) GetAccountInfoWithOpts(_ context.Context, _ solana.PublicKey, _ *rpc.GetAccountInfoOpts) (*rpc.GetAccountInfoResult, error) {
	return &rpc.GetAccountInfoResult{Value: &rpc.Account{Owner: token.ProgramID, Data: rpc.DataBytesOrJSONFromBytes(make([]byte, mintAccountSize))}}, nil
}

func (f *fakeRPC) GetMinimumBalanceForRentExemption(_ context.Context, _ uint64, _ rpc.CommitmentType) (uint64, error) {
	return 1_000_000, nil
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

// firingClock is a Timing whose timers fire immediately, so the confirm/wait loops do
// not sleep for real. It only advances deterministically via already-ready channels.
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

// TestCreateMintReturnsAddressOnUnconfirmedSubmit proves CreateMint hands back the mint
// address when the create transaction was submitted but never confirmed, so the caller
// can still revoke a mint that may exist on-chain. Without the fix CreateMint returns
// the zero address and the mint is stranded inflatable.
func TestCreateMintReturnsAddressOnUnconfirmedSubmit(t *testing.T) {
	f := &fakeRPC{confirm: false}
	eng := newTestEngine(f)

	mint, err := eng.CreateMint(context.Background(), 9)
	if err == nil {
		t.Fatal("expected a confirmation-timeout error")
	}
	if mint.IsZero() {
		t.Fatalf("CreateMint returned a zero address after submitting the create transaction; a stranded mint cannot be revoked (err=%v)", err)
	}
}

// TestMintRevokesAfterCancellationDuringFinalize proves the safety revoke is still
// submitted when the caller's context is canceled during finalize. Without the fix the
// cleanup revoke reuses the canceled context, never reaches the network, and the mint
// is left with a live mint authority.
func TestMintRevokesAfterCancellationDuringFinalize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// confirm=true so CreateMint (send #1) succeeds; send #2 is the metadata step,
	// where we cancel the caller context and fail, forcing the cleanup path.
	f := &fakeRPC{confirm: true, cancelOnSend: 2, cancel: cancel}
	eng := newTestEngine(f)

	_, _, err := eng.Mint(ctx, MintSpec{
		Name: "Flynn", Symbol: "FLYNN", MetadataURI: "https://example.com/token.json",
		Decimals: 9, Supply: 1,
	})
	if err == nil {
		t.Fatal("expected an error from the canceled finalize")
	}
	if !f.revokeSubmitted {
		t.Fatal("safety revoke was never submitted after the finalize failure; the created mint is left inflatable")
	}
}
