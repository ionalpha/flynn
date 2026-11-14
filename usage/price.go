package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Nanos is an amount of money in billionths of its currency unit. It marshals as
// a decimal string ("3.75", "0.000123") so a price table is written the way a
// price page states it and no amount passes through a float on the way in or out.
type Nanos int64

const nanosPerUnit = 1_000_000_000

// ParseNanos reads a non-negative decimal amount with at most nine digits after
// the point. A tenth digit is refused rather than rounded, because a rate that
// silently loses precision gives a cost that is wrong by an amount nobody can see.
func ParseNanos(s string) (Nanos, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" && frac == "" {
		return 0, fmt.Errorf("usage: amount %q is empty", s)
	}
	if len(frac) > 9 {
		return 0, fmt.Errorf("usage: amount %q has more than nine decimal places", s)
	}
	digits := func(p string) bool {
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	if !digits(whole) || !digits(frac) {
		return 0, fmt.Errorf("usage: amount %q is not a non-negative decimal", s)
	}
	var w, f int64
	var err error
	if whole != "" {
		if w, err = strconv.ParseInt(whole, 10, 64); err != nil || w > math.MaxInt64/nanosPerUnit {
			return 0, fmt.Errorf("usage: amount %q is out of range", s)
		}
	}
	if frac != "" {
		f, _ = strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
	}
	return Nanos(w*nanosPerUnit + f), nil
}

// String formats n as a decimal with trailing zeros after the point removed.
func (n Nanos) String() string {
	sign, whole, frac := "", int64(n)/nanosPerUnit, int64(n)%nanosPerUnit
	if n < 0 {
		sign, whole, frac = "-", -whole, -frac
	}
	s := fmt.Sprintf("%s%d.%09d", sign, whole, frac)
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

// MarshalText implements [encoding.TextMarshaler].
func (n Nanos) MarshalText() ([]byte, error) { return []byte(n.String()), nil }

// UnmarshalText implements [encoding.TextUnmarshaler] with [ParseNanos].
func (n *Nanos) UnmarshalText(b []byte) error {
	v, err := ParseNanos(string(b))
	if err != nil {
		return err
	}
	*n = v
	return nil
}

// Price is one dated row of a [Table]: what a model charged per million tokens
// from Effective onward, until a later row for the same model takes over. Input
// is charged on input not served from cache. CacheRead and CacheWrite are nil
// when the source states no rate for them; a call that used the cache is then
// unpriced rather than charged at a guessed rate. Source says where the rates
// came from (a price page URL and the date it was read), so a reader can check
// a cost back to its origin.
type Price struct {
	Provider   string
	Model      string
	Effective  time.Time
	Currency   string
	Input      Nanos
	Output     Nanos
	CacheRead  *Nanos
	CacheWrite *Nanos
	Source     string
}

// ID names the row as provider/model@date, the form a [Cost] cites.
func (p Price) ID() string {
	return p.Provider + "/" + p.Model + "@" + p.Effective.Format(time.DateOnly)
}

// priceJSON is the written form of a [Price]: the date as YYYY-MM-DD and the
// rates as decimal strings per million tokens.
type priceJSON struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Effective  string `json:"effective"`
	Currency   string `json:"currency"`
	Input      Nanos  `json:"input"`
	Output     Nanos  `json:"output"`
	CacheRead  *Nanos `json:"cacheRead,omitempty"`
	CacheWrite *Nanos `json:"cacheWrite,omitempty"`
	Source     string `json:"source"`
}

// MarshalJSON writes the row in the form [LoadTable] reads.
func (p Price) MarshalJSON() ([]byte, error) {
	return json.Marshal(priceJSON{
		Provider: p.Provider, Model: p.Model, Effective: p.Effective.Format(time.DateOnly),
		Currency: p.Currency, Input: p.Input, Output: p.Output,
		CacheRead: p.CacheRead, CacheWrite: p.CacheWrite, Source: p.Source,
	})
}

// UnmarshalJSON reads the form [Price.MarshalJSON] writes. Unknown fields are
// refused: a misspelt "cache_read" would otherwise drop a rate without a word,
// and every cached call would come out unpriced.
func (p *Price) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var w priceJSON
	if err := dec.Decode(&w); err != nil {
		return fmt.Errorf("usage: price row: %w", err)
	}
	eff, err := time.Parse(time.DateOnly, w.Effective)
	if err != nil {
		return fmt.Errorf("usage: price row %s/%s: effective date %q: want YYYY-MM-DD", w.Provider, w.Model, w.Effective)
	}
	*p = Price{
		Provider: w.Provider, Model: w.Model, Effective: eff, Currency: w.Currency,
		Input: w.Input, Output: w.Output, CacheRead: w.CacheRead, CacheWrite: w.CacheWrite,
		Source: w.Source,
	}
	return nil
}

func (p Price) validate() error {
	id := p.Provider + "/" + p.Model
	switch {
	case p.Provider == "" || p.Model == "":
		return fmt.Errorf("usage: price row %q needs a provider and a model", id)
	case p.Effective.IsZero():
		return fmt.Errorf("usage: price row %s has no effective date", id)
	case p.Currency == "":
		return fmt.Errorf("usage: price row %s has no currency", id)
	case p.Source == "":
		return fmt.Errorf("usage: price row %s has no source; a rate nobody can trace cannot be checked", id)
	case p.Input < 0 || p.Output < 0 ||
		(p.CacheRead != nil && *p.CacheRead < 0) || (p.CacheWrite != nil && *p.CacheWrite < 0):
		return fmt.Errorf("usage: price row %s has a negative rate", id)
	}
	return nil
}

// Table is a set of dated [Price] rows. It is read-only once built, so one table
// is safe to share across goroutines.
type Table struct {
	rows map[string][]Price // key: provider + "\x00" + model; each slice sorted by Effective
}

func tableKey(provider, model string) string { return provider + "\x00" + model }

// NewTable builds a table from rows. Each row's effective date is reduced to its
// UTC calendar day, the granularity price pages announce changes at. A row
// missing its provider, model, currency or source, a negative rate, or two rows
// for the same model on the same day is an error.
func NewTable(rows ...Price) (*Table, error) {
	t := &Table{rows: make(map[string][]Price)}
	for _, p := range rows {
		if err := p.validate(); err != nil {
			return nil, err
		}
		y, m, d := p.Effective.UTC().Date()
		p.Effective = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		k := tableKey(p.Provider, p.Model)
		t.rows[k] = append(t.rows[k], p)
	}
	for k, rs := range t.rows {
		sort.Slice(rs, func(i, j int) bool { return rs[i].Effective.Before(rs[j].Effective) })
		for i := 1; i < len(rs); i++ {
			if rs[i].Effective.Equal(rs[i-1].Effective) {
				return nil, fmt.Errorf("usage: two price rows for %s", rs[i].ID())
			}
		}
		t.rows[k] = rs
	}
	return t, nil
}

// tableJSON is the file form [LoadTable] reads.
type tableJSON struct {
	Prices []Price `json:"prices"`
}

// LoadTable reads a table from its JSON form, {"prices": [row, ...]}, with each
// row as [Price.MarshalJSON] writes it.
func LoadTable(r io.Reader) (*Table, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var w tableJSON
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("usage: price table: %w", err)
	}
	return NewTable(w.Prices...)
}

// Lookup returns the row in force for provider and model at time at: the latest
// row whose effective day is on or before at's UTC day. Model names match
// exactly; an alias or a dated snapshot name needs its own row.
func (t *Table) Lookup(provider, model string, at time.Time) (Price, bool) {
	if t == nil {
		return Price{}, false
	}
	rs := t.rows[tableKey(provider, model)]
	at = at.UTC()
	i := sort.Search(len(rs), func(i int) bool { return rs[i].Effective.After(at) })
	if i == 0 {
		return Price{}, false
	}
	return rs[i-1], true
}

// errOverflow is returned when a cost does not fit in [Nanos]; at nine billion
// currency units per call it means corrupt input, not a large bill.
var errOverflow = errors.New("usage: cost overflows")

// charge returns tokens at rate per million tokens, rounded half up to the
// nearest nano. The product is taken in 128 bits so no realistic token count or
// rate can overflow before the division.
func charge(tokens int64, rate Nanos) (Nanos, error) {
	if tokens <= 0 || rate <= 0 {
		// Validation refuses negative figures and rates before a charge is
		// taken, so only zero reaches here.
		return 0, nil
	}
	hi, lo := bits.Mul64(uint64(tokens), uint64(rate)) //nolint:gosec // G115: both are positive, checked above
	lo, carry := bits.Add64(lo, 500_000, 0)
	hi += carry
	if hi >= 1_000_000 {
		return 0, errOverflow
	}
	q, _ := bits.Div64(hi, lo, 1_000_000)
	if q > math.MaxInt64 {
		return 0, errOverflow
	}
	return Nanos(q), nil
}

// Price returns what r cost under the table: priced at the row in force on r.At,
// with basis [Derived] when every token figure was reported or derived and
// [Estimated] when any was estimated. A record the table cannot price comes back
// with Known false and a reason, never with a zero amount that reads as free.
func (t *Table) Price(r Record) Cost {
	unknown := func(format string, args ...any) Cost {
		return Cost{Reason: fmt.Sprintf(format, args...)}
	}
	if err := r.Tokens.Validate(); err != nil {
		return unknown("%v", err)
	}
	p, ok := t.Lookup(r.Provider, r.Model, r.At)
	if !ok {
		return unknown("no price for %s/%s on %s", r.Provider, r.Model, r.At.UTC().Format(time.DateOnly))
	}
	tk := r.Tokens
	if tk.CacheRead.N > 0 && p.CacheRead == nil {
		return unknown("%s states no cache-read rate and the call read %d cached tokens", p.ID(), tk.CacheRead.N)
	}
	if tk.CacheWrite.N > 0 && p.CacheWrite == nil {
		return unknown("%s states no cache-write rate and the call wrote %d cached tokens", p.ID(), tk.CacheWrite.N)
	}
	parts := []struct {
		n    int64
		rate *Nanos
	}{
		{tk.Input.N - tk.CacheRead.N, &p.Input},
		{tk.Output.N, &p.Output},
		{tk.CacheRead.N, p.CacheRead},
		{tk.CacheWrite.N, p.CacheWrite},
	}
	var total Nanos
	for _, part := range parts {
		if part.n == 0 {
			continue
		}
		c, err := charge(part.n, *part.rate)
		if err != nil || total > math.MaxInt64-c {
			return unknown("%s: %v", p.ID(), errOverflow)
		}
		total += c
	}
	cost := Cost{Known: true, Amount: total, Currency: p.Currency, Basis: Derived, Price: p.ID()}
	for _, c := range []Count{tk.Input, tk.Output, tk.CacheRead, tk.CacheWrite} {
		if c.N > 0 {
			cost.Basis = weaker(cost.Basis, c.Basis)
		}
	}
	if cost.Basis == Estimated {
		cost.Method = "priced from estimated token counts"
	}
	return cost
}
