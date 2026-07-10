// Package safety is the chain-agnostic anti-scam policy for token operations. It
// judges a described TokenPlan - not a live chain - so it has no crypto dependency
// and one rule set covers every chain. A chain adapter builds a plan for what it is
// about to do and calls Guard before it signs; a scam-shaped plan is refused and
// never reaches signing. Detection runs both ways: the same rules that forbid Flynn
// from minting a scam also flag one it is asked to inspect.
package safety

import (
	"fmt"
	"sort"
	"strings"
)

// Severity ranks a violation. Block stops the action; Warn requires disclosure but
// does not by itself stop a governed, human-signed action.
type Severity string

// Severity levels: Block stops the action, Warn only requires disclosure.
const (
	Block Severity = "block"
	Warn  Severity = "warn"
)

// Violation is one policy failure found in a plan.
type Violation struct {
	Code     string
	Severity Severity
	Detail   string
}

// TokenPlan is the chain-agnostic description of a token action the policy judges.
// Every field defaults to the safe value, so the zero plan is clean and a scam shape
// is an explicit opt-in that the policy then refuses.
type TokenPlan struct {
	Chain string // "solana", "evm", ... (informational; rules are chain-agnostic)
	Op    string // "mint", "manage", "remove_liquidity", ...

	// Retained powers - the core rug levers. Each true is the ability to harm holders.
	RetainedMintAuthority    bool   // can mint more later (inflation/dilution)
	RetainedFreezeAuthority  bool   // can freeze holders so they cannot sell (honeypot)
	PermanentDelegate        bool   // can transfer/burn ANY holder's tokens (burn scam)
	TransferHook             bool   // arbitrary code on every transfer (sell-block, hidden logic)
	Blacklist                bool   // blacklist/whitelist gating of transfers (honeypot)
	Pausable                 bool   // can halt all transfers (mostly EVM)
	RetainedUpgradeAuthority bool   // can change the token program/contract logic later
	TransferFeeBps           uint16 // transfer / buy-sell tax in basis points

	// Liquidity.
	SeedsLiquidity   bool // this action adds liquidity
	LPLockedOrBurned bool // ... and the LP is locked or burned (required if seeding)
	RemovesLiquidity bool // this action removes liquidity
	LiquidityLocked  bool // the pool is marked locked (so removal must be refused)

	// Economics / legal.
	GuaranteedYield   bool // wires guaranteed/fixed/issuer-set returns (ponzi + security)
	IssuerProfitClaim bool // generated copy/metadata claims price/profit/appreciation

	// Distribution + identity.
	CreatorSupplyPct uint8  // percent of supply the creator retains unvested
	Impersonates     string // a project/brand this token's identity impersonates, if any
}

// Evaluate returns every policy violation in a plan, most severe first. An empty
// result means the plan is clean.
func Evaluate(p TokenPlan) []Violation {
	var v []Violation
	block := func(code, detail string) { v = append(v, Violation{code, Block, detail}) }
	warn := func(code, detail string) { v = append(v, Violation{code, Warn, detail}) }

	if p.RetainedMintAuthority {
		block("retained_mint_authority", "mint authority not revoked: supply is not provably fixed (unlimited-mint dilution)")
	}
	if p.RetainedFreezeAuthority {
		block("retained_freeze_authority", "freeze authority retained: holders can be frozen out of selling (honeypot)")
	}
	if p.PermanentDelegate {
		block("permanent_delegate", "permanent delegate set: any holder's tokens can be transferred or burned without consent")
	}
	if p.TransferHook {
		block("transfer_hook", "transfer hook present: arbitrary code runs on every transfer (sell-block risk)")
	}
	if p.Blacklist {
		block("transfer_blacklist", "blacklist/whitelist transfer gating: sells can be selectively blocked (honeypot)")
	}
	if p.Pausable {
		block("pausable", "transfers are pausable: all trading can be halted by the owner")
	}
	if p.RetainedUpgradeAuthority {
		block("retained_upgrade_authority", "token program/contract upgrade authority retained: token logic can be changed later")
	}
	if p.TransferFeeBps > 0 {
		block("transfer_fee", fmt.Sprintf("transfer fee of %d bps: a transfer tax traps value and is honeypot-adjacent", p.TransferFeeBps))
	}
	if p.SeedsLiquidity && !p.LPLockedOrBurned {
		block("unlocked_lp", "seeding liquidity without locking or burning the LP: liquidity can be pulled (rug)")
	}
	if p.RemovesLiquidity && p.LiquidityLocked {
		block("remove_locked_liquidity", "removing liquidity from a pool marked locked: this is the rug action itself")
	}
	if p.GuaranteedYield {
		block("guaranteed_yield", "guaranteed/fixed/issuer-set yield wired in: ponzi shape and a securities trigger")
	}
	if p.IssuerProfitClaim {
		block("issuer_profit_claim", "generated copy/metadata claims price/profit/appreciation: pump framing and a Howey trigger")
	}
	if strings.TrimSpace(p.Impersonates) != "" {
		block("impersonation", "token identity impersonates "+p.Impersonates+": brand/impersonation scam")
	}
	if p.CreatorSupplyPct > 30 {
		warn("creator_concentration", fmt.Sprintf("creator retains %d%% of supply unvested: dump risk, must be disclosed and vested", p.CreatorSupplyPct))
	}

	sort.SliceStable(v, func(i, j int) bool { return v[i].Severity == Block && v[j].Severity != Block })
	return v
}

// Guard evaluates a plan and returns a non-nil error naming every blocking
// violation, or nil if the plan carries none. Warn-level findings do not block; the
// caller surfaces them for disclosure. This is the gate every chain adapter calls
// before it signs: a scam-shaped plan cannot proceed.
func Guard(p TokenPlan) error {
	var blocking []string
	for _, viol := range Evaluate(p) {
		if viol.Severity == Block {
			blocking = append(blocking, viol.Code+": "+viol.Detail)
		}
	}
	if len(blocking) == 0 {
		return nil
	}
	return fmt.Errorf("token safety policy refused this action (%d blocking):\n  - %s",
		len(blocking), strings.Join(blocking, "\n  - "))
}
