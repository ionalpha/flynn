// Package usage is the account of what model calls consumed: one [Record] per call
// (or per external episode, when the agent that ran it reports nothing finer), the
// tokens it used, what they cost, and how each figure is known.
//
// A token count reaches Flynn by different routes. A native adapter reads it from
// the provider's response; an external agent such as a coding CLI prints it in its
// stream, sometimes as a running total and sometimes not at all for output; a host
// restating an old run may rebuild it from a partial split. A figure that was
// counted and one that was guessed look the same as integers, and a report that
// sums them without saying which is which states more than it knows. Each figure
// therefore carries a [Basis], and an [Estimated] figure names its method.
//
// Cost is computed, never reported, from a [Table] of dated prices. A call is
// priced at the rate in force on the day it ran, so a later price change does not
// rewrite the cost of earlier calls. A call the table cannot price has an unknown
// cost, which is different from a zero cost: a free local model is priced at zero
// by a row that says so, and a model nobody priced is reported as unpriced so a
// total cannot look complete when it is not.
//
// Amounts are integer [Nanos] (billionths of the currency unit) so that summing a
// long run's calls is exact and two hosts summing the same records agree to the
// last digit, which float arithmetic does not promise.
package usage

import (
	"errors"
	"fmt"
	"time"

	"github.com/ionalpha/flynn/llm"
)

// Basis says how a figure is known.
type Basis string

const (
	// Reported means the provider or the agent that made the call stated the figure.
	Reported Basis = "reported"
	// Derived means the figure was computed exactly from reported figures, as a
	// cost is from reported tokens and a dated rate.
	Derived Basis = "derived"
	// Estimated means the figure is an approximation; the [Count] or [Cost] that
	// carries it must name the method so a reader can judge it.
	Estimated Basis = "estimated"
)

// valid reports whether b is one of the defined bases. The empty basis is not
// valid: a figure with no stated basis is the ambiguity this package exists to
// remove.
func (b Basis) valid() bool {
	switch b {
	case Reported, Derived, Estimated:
		return true
	default:
		return false
	}
}

// weaker returns the less certain of two bases, so a figure computed from an
// estimate is itself an estimate.
func weaker(a, b Basis) Basis {
	rank := func(x Basis) int {
		switch x {
		case Reported:
			return 0
		case Derived:
			return 1
		default:
			return 2
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// Count is one token figure and how it is known. Method is required when Basis is
// [Estimated] and says how the estimate was made ("recounted from stream deltas;
// output understated", "scaled from a partial per-model split").
type Count struct {
	N      int64  `json:"n"`
	Basis  Basis  `json:"basis"`
	Method string `json:"method,omitempty"`
}

func (c Count) validate(name string) error {
	if c.N < 0 {
		return fmt.Errorf("usage: %s is negative (%d)", name, c.N)
	}
	if !c.Basis.valid() {
		return fmt.Errorf("usage: %s has basis %q; want reported, derived or estimated", name, c.Basis)
	}
	if c.Basis == Estimated && c.Method == "" {
		return fmt.Errorf("usage: %s is estimated with no method", name)
	}
	return nil
}

// Tokens is the four token figures of one call, with the meanings [llm.Usage]
// defines: Input is all input processed, including any part served from cache;
// CacheRead is the part of Input served from a prompt cache; CacheWrite is input
// written to the cache, in addition to Input; Output is generated tokens.
type Tokens struct {
	Input      Count `json:"input"`
	Output     Count `json:"output"`
	CacheRead  Count `json:"cacheRead"`
	CacheWrite Count `json:"cacheWrite"`
}

// FromLLM returns the tokens a native adapter reported, each with basis
// [Reported].
func FromLLM(u llm.Usage) Tokens {
	r := func(n int) Count { return Count{N: int64(n), Basis: Reported} }
	return Tokens{
		Input:      r(u.InputTokens),
		Output:     r(u.OutputTokens),
		CacheRead:  r(u.CacheReadTokens),
		CacheWrite: r(u.CacheWriteTokens),
	}
}

// Validate reports the first figure that is negative, has no valid basis, or is
// estimated without a method, and refuses a cache read larger than the input it
// is a part of.
func (t Tokens) Validate() error {
	for _, f := range []struct {
		name string
		c    Count
	}{{"input", t.Input}, {"output", t.Output}, {"cacheRead", t.CacheRead}, {"cacheWrite", t.CacheWrite}} {
		if err := f.c.validate(f.name); err != nil {
			return err
		}
	}
	if t.CacheRead.N > t.Input.N {
		return fmt.Errorf("usage: cacheRead (%d) exceeds input (%d), which includes it", t.CacheRead.N, t.Input.N)
	}
	return nil
}

// Total is input plus cache writes plus output: every token the call was billed
// for in some form. Cache reads are already inside Input.
func (t Tokens) Total() int64 { return t.Input.N + t.CacheWrite.N + t.Output.N }

// Cost is what a call cost, or the fact that it could not be priced. Known false
// means unpriced: Amount is then zero and means nothing, and Reason says why
// (no row for the model, a figure the row has no rate for). Price names the row
// that priced the call, so a reader can check the rate and its source.
type Cost struct {
	Known    bool   `json:"known"`
	Amount   Nanos  `json:"amount,omitempty"`
	Currency string `json:"currency,omitempty"`
	Basis    Basis  `json:"basis,omitempty"`
	Method   string `json:"method,omitempty"`
	Price    string `json:"price,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Record is the account of one model call. Run and ParentRun place it in the
// spawn tree so a total can be summed up the tree; Principal is who the call was
// made for; Call identifies the call or episode within the run. At is when the
// call ran, and is the date its price is looked up for.
type Record struct {
	Run       string    `json:"run"`
	ParentRun string    `json:"parentRun,omitempty"`
	Principal string    `json:"principal,omitempty"`
	Call      string    `json:"call"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	At        time.Time `json:"at"`
	Tokens    Tokens    `json:"tokens"`
	Cost      Cost      `json:"cost"`
}

// Validate reports a record missing what makes it attributable (run, call,
// provider, model, time) or carrying an invalid token figure.
func (r Record) Validate() error {
	switch {
	case r.Run == "":
		return errors.New("usage: record has no run")
	case r.Call == "":
		return errors.New("usage: record has no call")
	case r.Provider == "":
		return errors.New("usage: record has no provider")
	case r.Model == "":
		return errors.New("usage: record has no model")
	case r.At.IsZero():
		return errors.New("usage: record has no time")
	}
	return r.Tokens.Validate()
}
