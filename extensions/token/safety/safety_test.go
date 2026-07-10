package safety

import "testing"

// cleanMint is the shape Flynn's own engine produces: revoked mint authority, no
// freeze, no extensions, no fee, no yield or profit claim. It must pass the policy.
func cleanMint() TokenPlan {
	return TokenPlan{Chain: "solana", Op: "mint", CreatorSupplyPct: 10}
}

func TestCleanPlanPasses(t *testing.T) {
	if err := Guard(cleanMint()); err != nil {
		t.Fatalf("clean mint should pass the safety policy, got: %v", err)
	}
	if vs := Evaluate(cleanMint()); len(vs) != 0 {
		t.Fatalf("clean mint should have no violations, got %v", vs)
	}
}

// Each mutation takes the clean plan and flips exactly one dangerous property. The
// policy must refuse every one (Guard returns an error) and report the expected
// code. If a rule is removed, the matching case fails - the mutation guarantee.
func TestEveryScamShapeIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*TokenPlan)
		wantErr string // expected violation code
	}{
		{"retained mint authority", func(p *TokenPlan) { p.RetainedMintAuthority = true }, "retained_mint_authority"},
		{"retained freeze authority", func(p *TokenPlan) { p.RetainedFreezeAuthority = true }, "retained_freeze_authority"},
		{"permanent delegate", func(p *TokenPlan) { p.PermanentDelegate = true }, "permanent_delegate"},
		{"transfer hook", func(p *TokenPlan) { p.TransferHook = true }, "transfer_hook"},
		{"blacklist gating", func(p *TokenPlan) { p.Blacklist = true }, "transfer_blacklist"},
		{"pausable transfers", func(p *TokenPlan) { p.Pausable = true }, "pausable"},
		{"retained upgrade authority", func(p *TokenPlan) { p.RetainedUpgradeAuthority = true }, "retained_upgrade_authority"},
		{"transfer fee", func(p *TokenPlan) { p.TransferFeeBps = 500 }, "transfer_fee"},
		{"unlocked LP", func(p *TokenPlan) { p.SeedsLiquidity = true; p.LPLockedOrBurned = false }, "unlocked_lp"},
		{"remove locked liquidity", func(p *TokenPlan) { p.RemovesLiquidity = true; p.LiquidityLocked = true }, "remove_locked_liquidity"},
		{"guaranteed yield", func(p *TokenPlan) { p.GuaranteedYield = true }, "guaranteed_yield"},
		{"issuer profit claim", func(p *TokenPlan) { p.IssuerProfitClaim = true }, "issuer_profit_claim"},
		{"impersonation", func(p *TokenPlan) { p.Impersonates = "USDC" }, "impersonation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := cleanMint()
			c.mutate(&p)
			if err := Guard(p); err == nil {
				t.Fatalf("%s: policy must REFUSE this plan, but Guard passed it", c.name)
			}
			found := false
			for _, v := range Evaluate(p) {
				if v.Code == c.wantErr {
					if v.Severity != Block {
						t.Fatalf("%s: violation %q must be Block, got %q", c.name, c.wantErr, v.Severity)
					}
					found = true
				}
			}
			if !found {
				t.Fatalf("%s: expected a %q blocking violation, got %v", c.name, c.wantErr, Evaluate(p))
			}
		})
	}
}

// Seeding liquidity WITH a locked/burned LP is legitimate and must pass.
func TestLockedLiquidityPasses(t *testing.T) {
	p := cleanMint()
	p.SeedsLiquidity = true
	p.LPLockedOrBurned = true
	if err := Guard(p); err != nil {
		t.Fatalf("seeding with a locked LP should pass, got: %v", err)
	}
}

// High creator concentration warns (disclosure/vesting) but does not by itself block
// a governed action - a fresh mint sends all supply to a treasury, which is normal.
func TestCreatorConcentrationWarnsNotBlocks(t *testing.T) {
	p := cleanMint()
	p.CreatorSupplyPct = 90
	if err := Guard(p); err != nil {
		t.Fatalf("high creator concentration should warn, not block, got: %v", err)
	}
	warned := false
	for _, v := range Evaluate(p) {
		if v.Code == "creator_concentration" && v.Severity == Warn {
			warned = true
		}
	}
	if !warned {
		t.Fatal("expected a creator_concentration warning")
	}
}
