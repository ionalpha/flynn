package sandbox

import (
	"context"
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
)

// tier is a sandbox stub that declares a containment level, so the gate and selector
// can be tested without a real isolation backend.
type tier struct{ level Containment }

func (t tier) Exec(context.Context, Command) (ExecResult, error) { return ExecResult{}, nil }
func (t tier) ReadFile(context.Context, string) ([]byte, error)  { return nil, nil }
func (t tier) WriteFile(context.Context, string, []byte) error   { return nil }
func (t tier) Glob(context.Context, string) ([]string, error)    { return nil, nil }
func (t tier) Walk(context.Context, string) ([]string, error)    { return nil, nil }
func (t tier) Close() error                                      { return nil }
func (t tier) Containment() Containment                          { return t.level }

// undeclared is a Sandbox with no Contained capability, to check the safe default.
type undeclared struct{}

func (undeclared) Exec(context.Context, Command) (ExecResult, error) { return ExecResult{}, nil }
func (undeclared) ReadFile(context.Context, string) ([]byte, error)  { return nil, nil }
func (undeclared) WriteFile(context.Context, string, []byte) error   { return nil }
func (undeclared) Glob(context.Context, string) ([]string, error)    { return nil, nil }
func (undeclared) Walk(context.Context, string) ([]string, error)    { return nil, nil }
func (undeclared) Close() error                                      { return nil }

func TestRequiredOrdering(t *testing.T) {
	if Required(TrustTrusted) > Required(TrustSemi) || Required(TrustSemi) > Required(TrustUntrusted) {
		t.Fatal("required containment must be non-decreasing in trust")
	}
	if Required(TrustUntrusted) != ContainmentMicroVM {
		t.Fatalf("untrusted work must require a hardware boundary by default, got %s", Required(TrustUntrusted))
	}
}

func TestContainmentOfDefaultsToWeakest(t *testing.T) {
	if got := ContainmentOf(undeclared{}); got != ContainmentNone {
		t.Fatalf("a sandbox that declares nothing must default to the weakest level, got %s", got)
	}
	if got := ContainmentOf(tier{ContainmentMicroVM}); got != ContainmentMicroVM {
		t.Fatalf("a declared level must be reported, got %s", got)
	}
}

func TestLocalIsProcessJailOnly(t *testing.T) {
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if ContainmentOf(l) != ContainmentNone {
		t.Fatal("the local tier must report process-jail-only containment")
	}
	// And the gate must refuse untrusted work on it.
	if Admit(l, TrustUntrusted) == nil {
		t.Fatal("the local tier must not admit untrusted work")
	}
	if Admit(l, TrustTrusted) != nil {
		t.Fatal("the local tier must admit trusted work")
	}
}

func TestAdmitGate(t *testing.T) {
	cases := []struct {
		level Containment
		trust Trust
		ok    bool
	}{
		{ContainmentNone, TrustTrusted, true},
		{ContainmentNone, TrustSemi, false},
		{ContainmentNone, TrustUntrusted, false},
		{ContainmentKernel, TrustSemi, true},
		{ContainmentKernel, TrustUntrusted, false},
		{ContainmentUserKernel, TrustUntrusted, false}, // strict: untrusted needs a hardware boundary
		{ContainmentMicroVM, TrustUntrusted, true},
		{ContainmentRemote, TrustUntrusted, true},
	}
	for _, c := range cases {
		err := Admit(tier{c.level}, c.trust)
		if (err == nil) != c.ok {
			t.Fatalf("Admit(%s, %s): ok=%v, want %v (err=%v)", c.level, trustName(c.trust), err == nil, c.ok, err)
		}
		if err != nil && !errors.Is(err, ErrInsufficientContainment) {
			t.Fatalf("refusal must wrap ErrInsufficientContainment, got %v", err)
		}
		if err != nil && fault.Classify(err) != fault.Forbidden {
			t.Fatalf("a refusal must be Forbidden, got %s", fault.Classify(err))
		}
	}
}

func TestSelectStrongestOrRefuse(t *testing.T) {
	weak := tier{ContainmentNone}
	mid := tier{ContainmentKernel}
	strong := tier{ContainmentMicroVM}

	// Untrusted: only the microVM qualifies; it is chosen.
	got, err := Select(TrustUntrusted, weak, mid, strong)
	if err != nil || ContainmentOf(got) != ContainmentMicroVM {
		t.Fatalf("untrusted must select the microVM, got %v err=%v", got, err)
	}
	// Semi-trusted: kernel and microVM qualify; the strongest (microVM) is chosen.
	got, err = Select(TrustSemi, weak, mid, strong)
	if err != nil || ContainmentOf(got) != ContainmentMicroVM {
		t.Fatalf("select must pick the strongest qualifying tier, got %v err=%v", got, err)
	}
	// No qualifying tier: refuse.
	if _, err := Select(TrustUntrusted, weak, mid); err == nil || !errors.Is(err, ErrInsufficientContainment) {
		t.Fatalf("select must refuse when nothing is strong enough, got %v", err)
	}
	// No candidates: refuse.
	if _, err := Select(TrustTrusted); err == nil {
		t.Fatal("select with no candidates must refuse")
	}
	// nil candidates are skipped.
	if got, err := Select(TrustTrusted, nil, weak); err != nil || ContainmentOf(got) != ContainmentNone {
		t.Fatalf("nil candidates must be skipped, got %v err=%v", got, err)
	}
}

// TestGateProperties pins the gate and selector contracts over random levels and
// trust: Admit succeeds exactly when the level meets the requirement, and Select
// returns the strongest qualifying tier or refuses when there is none, never a tier
// below the requirement.
func TestGateProperties(t *testing.T) {
	level := func(rt *rapid.T, label string) Containment {
		return Containment(rapid.IntRange(int(ContainmentNone), int(ContainmentRemote)).Draw(rt, label))
	}
	trust := func(rt *rapid.T) Trust {
		return Trust(rapid.IntRange(int(TrustTrusted), int(TrustUntrusted)).Draw(rt, "trust"))
	}

	rapid.Check(t, func(rt *rapid.T) {
		tr := trust(rt)

		// Admit agrees with the order relation exactly.
		l := level(rt, "one")
		wantAdmit := l >= Required(tr)
		if (Admit(tier{l}, tr) == nil) != wantAdmit {
			rt.Fatalf("Admit disagreed: level=%s trust=%s", l, trustName(tr))
		}

		// Select over a random set of tiers.
		n := rapid.IntRange(0, 6).Draw(rt, "n")
		cands := make([]Sandbox, 0, n)
		maxQualifying := Containment(-1)
		anyQualifies := false
		for range n {
			lv := level(rt, "lv")
			cands = append(cands, tier{lv})
			if lv >= Required(tr) {
				anyQualifies = true
				if lv > maxQualifying {
					maxQualifying = lv
				}
			}
		}
		got, err := Select(tr, cands...)
		if anyQualifies {
			if err != nil {
				rt.Fatalf("Select refused though a tier qualified (trust=%s)", trustName(tr))
			}
			if ContainmentOf(got) != maxQualifying {
				rt.Fatalf("Select returned %s, want the strongest qualifying %s", ContainmentOf(got), maxQualifying)
			}
			if ContainmentOf(got) < Required(tr) {
				rt.Fatal("Select returned a tier below the requirement")
			}
		} else if err == nil {
			rt.Fatalf("Select admitted with no qualifying tier (trust=%s)", trustName(tr))
		}
	})
}
