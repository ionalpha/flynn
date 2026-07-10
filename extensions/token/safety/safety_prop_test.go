package safety

import (
	"testing"

	"pgregory.net/rapid"
)

// drawPlan generates an arbitrary TokenPlan across the whole option space.
func drawPlan(t *rapid.T) TokenPlan {
	return TokenPlan{
		Chain:                    rapid.SampledFrom([]string{"solana", "evm", ""}).Draw(t, "chain"),
		Op:                       rapid.SampledFrom([]string{"mint", "manage", "remove_liquidity"}).Draw(t, "op"),
		RetainedMintAuthority:    rapid.Bool().Draw(t, "mintAuth"),
		RetainedFreezeAuthority:  rapid.Bool().Draw(t, "freezeAuth"),
		PermanentDelegate:        rapid.Bool().Draw(t, "permDelegate"),
		TransferHook:             rapid.Bool().Draw(t, "hook"),
		Blacklist:                rapid.Bool().Draw(t, "blacklist"),
		Pausable:                 rapid.Bool().Draw(t, "pausable"),
		RetainedUpgradeAuthority: rapid.Bool().Draw(t, "upgrade"),
		TransferFeeBps:           rapid.Uint16().Draw(t, "fee"),
		SeedsLiquidity:           rapid.Bool().Draw(t, "seeds"),
		LPLockedOrBurned:         rapid.Bool().Draw(t, "lpLocked"),
		RemovesLiquidity:         rapid.Bool().Draw(t, "removes"),
		LiquidityLocked:          rapid.Bool().Draw(t, "poolLocked"),
		GuaranteedYield:          rapid.Bool().Draw(t, "yield"),
		IssuerProfitClaim:        rapid.Bool().Draw(t, "profit"),
		CreatorSupplyPct:         rapid.Uint8Range(0, 100).Draw(t, "creatorPct"),
		Impersonates:             rapid.SampledFrom([]string{"", "USDC", "Flynn"}).Draw(t, "impersonates"),
	}
}

// TestGuardBlocksExactlyOnBlockingViolation checks the core invariant across the
// whole plan space: Guard refuses a plan if and only if Evaluate finds at least one
// Block-severity violation. Warn-only findings never block.
func TestGuardBlocksExactlyOnBlockingViolation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := drawPlan(t)
		blocking := false
		for _, v := range Evaluate(p) {
			if v.Severity == Block {
				blocking = true
			}
		}
		err := Guard(p)
		if blocking != (err != nil) {
			t.Fatalf("Guard/Evaluate disagree: blocking=%v, Guard err=%v, plan=%+v", blocking, err, p)
		}
	})
}

// TestAnyBlockingLeverIsRefused checks that whenever a plan sets any single blocking
// lever, Guard refuses it, no matter what the other fields are.
func TestAnyBlockingLeverIsRefused(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		p := drawPlan(t)
		// Force one known-blocking lever on; Guard must then refuse regardless of the rest.
		p.PermanentDelegate = true
		if err := Guard(p); err == nil {
			t.Fatalf("permanent delegate set but Guard passed: plan=%+v", p)
		}
	})
}
