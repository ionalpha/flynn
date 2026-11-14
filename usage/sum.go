package usage

import (
	"fmt"
	"maps"
	"math"
	"slices"
)

// Summary is the total of a set of records. Costs holds one amount per currency,
// because amounts in different currencies cannot be added without a dated
// exchange rate this package does not have. Unpriced counts the records whose
// cost was unknown; while it is above zero the cost totals are a floor, and
// [Summary.Complete] says so.
type Summary struct {
	Calls     int              `json:"calls"`
	Tokens    Tokens           `json:"tokens"`
	Costs     map[string]Nanos `json:"costs,omitempty"`
	CostBasis Basis            `json:"costBasis,omitempty"`
	Unpriced  int              `json:"unpriced,omitempty"`
}

// Complete reports whether every summed record was priced, so the cost totals
// are the whole cost and not a part of it.
func (s Summary) Complete() bool { return s.Unpriced == 0 }

// Currencies returns the currencies in Costs, sorted, for stable output.
func (s Summary) Currencies() []string { return slices.Sorted(maps.Keys(s.Costs)) }

// add folds one count into a running total. The total's basis is the weakest of
// its parts, so a sum that includes one estimate is an estimate.
func (c *Count) add(o Count) {
	c.N += o.N
	if c.Basis == "" {
		c.Basis = Derived
	}
	c.Basis = weaker(c.Basis, o.Basis)
	if c.Basis == Reported {
		c.Basis = Derived
	}
	if c.Basis == Estimated {
		c.Method = "sum including estimated figures"
	}
}

// Sum totals records. Token figures add with the weakest basis among them;
// costs add per currency; an unpriced record adds its tokens and counts toward
// Unpriced. It refuses a record whose tokens fail [Tokens.Validate], since a
// figure with no basis would make the total's basis unknowable, and returns an
// error if a currency's total overflows [Nanos].
func Sum(records []Record) (Summary, error) {
	s := Summary{Calls: len(records)}
	for i, r := range records {
		if err := r.Tokens.Validate(); err != nil {
			return Summary{}, fmt.Errorf("usage: record %d (%s): %w", i, r.Call, err)
		}
		s.Tokens.Input.add(r.Tokens.Input)
		s.Tokens.Output.add(r.Tokens.Output)
		s.Tokens.CacheRead.add(r.Tokens.CacheRead)
		s.Tokens.CacheWrite.add(r.Tokens.CacheWrite)
		if !r.Cost.Known {
			s.Unpriced++
			continue
		}
		if s.Costs == nil {
			s.Costs = make(map[string]Nanos)
		}
		prev := s.Costs[r.Cost.Currency]
		if r.Cost.Amount > 0 && prev > math.MaxInt64-r.Cost.Amount {
			return Summary{}, fmt.Errorf("usage: %s total overflows", r.Cost.Currency)
		}
		s.Costs[r.Cost.Currency] = prev + r.Cost.Amount
		if s.CostBasis == "" {
			s.CostBasis = r.Cost.Basis
		}
		s.CostBasis = weaker(s.CostBasis, r.Cost.Basis)
	}
	return s, nil
}
