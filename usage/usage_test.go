package usage_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/usage"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func nanos(t *testing.T, s string) usage.Nanos {
	t.Helper()
	n, err := usage.ParseNanos(s)
	if err != nil {
		t.Fatalf("ParseNanos(%q): %v", s, err)
	}
	return n
}

func ptr(n usage.Nanos) *usage.Nanos { return &n }

func reported(in, out, cacheRead, cacheWrite int) usage.Tokens {
	return usage.FromLLM(llm.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite})
}

func call(model string, at time.Time, tk usage.Tokens) usage.Record {
	return usage.Record{Run: "r1", Call: "c1", Provider: "acme", Model: model, At: at, Tokens: tk}
}

// table is a fictional provider whose rates change on 1 Nov, so tests cannot be
// read as a statement about any real provider's prices.
func table(t *testing.T) *usage.Table {
	t.Helper()
	tb, err := usage.NewTable(
		usage.Price{
			Provider: "acme", Model: "m1", Effective: day("2026-10-01"), Currency: "USD",
			Input: nanos(t, "3"), Output: nanos(t, "15"), CacheRead: ptr(nanos(t, "0.30")), CacheWrite: ptr(nanos(t, "3.75")),
			Source: "test fixture",
		},
		usage.Price{
			Provider: "acme", Model: "m1", Effective: day("2026-11-01"), Currency: "USD",
			Input: nanos(t, "1"), Output: nanos(t, "5"), CacheRead: ptr(nanos(t, "0.10")), CacheWrite: ptr(nanos(t, "1.25")),
			Source: "test fixture",
		},
		usage.Price{
			Provider: "acme", Model: "nocache", Effective: day("2026-10-01"), Currency: "USD",
			Input: nanos(t, "2"), Output: nanos(t, "8"), Source: "test fixture",
		},
		usage.Price{
			Provider: "local", Model: "free", Effective: day("2026-01-01"), Currency: "USD",
			Source: "runs on this machine",
		},
	)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	return tb
}

func TestParseNanos(t *testing.T) {
	for _, c := range []struct {
		in   string
		want usage.Nanos
		ok   bool
	}{
		{"3", 3_000_000_000, true},
		{"0.30", 300_000_000, true},
		{"3.75", 3_750_000_000, true},
		{".5", 500_000_000, true},
		{"0.000000001", 1, true},
		{"0", 0, true},
		{"", 0, false},
		{".", 0, false},
		{"-1", 0, false},
		{"1e3", 0, false},
		{"0.0000000001", 0, false},
		{"9223372037", 0, false},
	} {
		got, err := usage.ParseNanos(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParseNanos(%q) = %d, %v; want %d, ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func TestNanosString(t *testing.T) {
	for n, want := range map[usage.Nanos]string{0: "0", 1: "0.000000001", 3_750_000_000: "3.75", 15_000_000_000: "15", -500_000_000: "-0.5"} {
		if got := n.String(); got != want {
			t.Errorf("Nanos(%d).String() = %q; want %q", int64(n), got, want)
		}
	}
}

// The task's acceptance case: the same call priced either side of a price change
// gives two different answers, each from the row in force that day.
func TestPriceFollowsTheRateInForceOnTheCallsDay(t *testing.T) {
	tb := table(t)
	tk := reported(1_000_000, 100_000, 400_000, 50_000)
	for _, c := range []struct {
		at   time.Time
		want string
		row  string
	}{
		// 600k uncached input at 3, 100k output at 15, 400k cache read at 0.30,
		// 50k cache write at 3.75: 1.8 + 1.5 + 0.12 + 0.1875.
		{day("2026-10-31").Add(23*time.Hour + 59*time.Minute), "3.6075", "acme/m1@2026-10-01"},
		// The same call at a third of the price, from 1 Nov onward.
		{day("2026-11-01"), "1.2025", "acme/m1@2026-11-01"},
		{day("2027-03-01"), "1.2025", "acme/m1@2026-11-01"},
	} {
		got := tb.Price(call("m1", c.at, tk))
		if !got.Known || got.Amount.String() != c.want || got.Price != c.row || got.Currency != "USD" || got.Basis != usage.Derived {
			t.Errorf("at %s: %+v; want %s USD derived from %s", c.at, got, c.want, c.row)
		}
	}
}

func TestPriceUsesTheCallsUTCDay(t *testing.T) {
	tb := table(t)
	// 1 Nov 06:00 in UTC+7 is still 31 Oct in UTC, so the October row applies.
	at := time.Date(2026, 11, 1, 6, 0, 0, 0, time.FixedZone("UTC+7", 7*3600))
	if got := tb.Price(call("m1", at, reported(1_000_000, 0, 0, 0))); got.Price != "acme/m1@2026-10-01" {
		t.Errorf("priced from %s; want the October row", got.Price)
	}
}

func TestUnpricedIsUnknownNotZero(t *testing.T) {
	tb := table(t)
	for name, r := range map[string]usage.Record{
		"unknown model":        call("nobody-priced-this", day("2026-11-02"), reported(10, 10, 0, 0)),
		"before the first row": call("m1", day("2026-09-30"), reported(10, 10, 0, 0)),
		"cache read, no rate":  call("nocache", day("2026-11-02"), reported(10, 10, 5, 0)),
		"cache write, no rate": call("nocache", day("2026-11-02"), reported(10, 10, 0, 5)),
		"invalid tokens":       call("m1", day("2026-11-02"), reported(10, 10, 20, 0)),
		"no time":              call("m1", time.Time{}, reported(10, 10, 0, 0)),
	} {
		got := tb.Price(r)
		if got.Known || got.Amount != 0 || got.Reason == "" {
			t.Errorf("%s: %+v; want unknown with a reason", name, got)
		}
	}
	var none *usage.Table
	if got := none.Price(call("m1", day("2026-11-02"), reported(1, 1, 0, 0))); got.Known {
		t.Errorf("nil table priced a call: %+v", got)
	}
}

func TestPriceWithoutCacheUseNeedsNoCacheRate(t *testing.T) {
	got := table(t).Price(call("nocache", day("2026-11-02"), reported(1_000_000, 1_000_000, 0, 0)))
	if !got.Known || got.Amount.String() != "10" {
		t.Errorf("%+v; want 10", got)
	}
}

func TestAFreeModelIsPricedAtZero(t *testing.T) {
	r := call("free", day("2026-11-02"), reported(5000, 5000, 0, 0))
	r.Provider = "local"
	got := table(t).Price(r)
	if !got.Known || got.Amount != 0 {
		t.Errorf("%+v; want known and zero", got)
	}
}

func TestCostFromAnEstimateIsAnEstimate(t *testing.T) {
	tk := reported(1000, 0, 0, 0)
	tk.Output = usage.Count{N: 500, Basis: usage.Estimated, Method: "recounted from stream deltas; output understated"}
	got := table(t).Price(call("m1", day("2026-11-02"), tk))
	if !got.Known || got.Basis != usage.Estimated || got.Method == "" {
		t.Errorf("%+v; want an estimated cost with a method", got)
	}
}

func TestChargeRoundsHalfUp(t *testing.T) {
	tb, err := usage.NewTable(usage.Price{
		Provider: "acme", Model: "tiny", Effective: day("2026-01-01"), Currency: "USD",
		Input: 1_500_000, Output: 1_499_999, Source: "test fixture", // 1.5 and 1.499999 nanos per token
	})
	if err != nil {
		t.Fatal(err)
	}
	// One input token is 1.5 nanos, rounded up to 2; one output token is just under
	// 1.5, rounded down to 1.
	got := tb.Price(call("tiny", day("2026-02-01"), reported(1, 1, 0, 0)))
	if got.Amount != 3 {
		t.Errorf("amount %d; want 3", got.Amount)
	}
}

func TestChargeOverflowIsUnknown(t *testing.T) {
	tb, err := usage.NewTable(usage.Price{
		Provider: "acme", Model: "dear", Effective: day("2026-01-01"), Currency: "USD",
		Input: 1 << 62, Output: 0, Source: "test fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := tb.Price(call("dear", day("2026-02-01"), reported(1<<40, 0, 0, 0)))
	if got.Known || !strings.Contains(got.Reason, "overflow") {
		t.Errorf("%+v; want unknown on overflow", got)
	}
}

func TestNewTableRefuses(t *testing.T) {
	ok := usage.Price{Provider: "acme", Model: "m", Effective: day("2026-01-01"), Currency: "USD", Input: 1, Output: 1, Source: "s"}
	for name, mut := range map[string]func(*usage.Price){
		"no provider":         func(p *usage.Price) { p.Provider = "" },
		"no model":            func(p *usage.Price) { p.Model = "" },
		"no date":             func(p *usage.Price) { p.Effective = time.Time{} },
		"no currency":         func(p *usage.Price) { p.Currency = "" },
		"no source":           func(p *usage.Price) { p.Source = "" },
		"negative input":      func(p *usage.Price) { p.Input = -1 },
		"negative cache read": func(p *usage.Price) { p.CacheRead = ptr(-1) },
	} {
		p := ok
		mut(&p)
		if _, err := usage.NewTable(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Two rows on the same UTC day for one model, even at different hours.
	later := ok
	later.Effective = ok.Effective.Add(5 * time.Hour)
	if _, err := usage.NewTable(ok, later); err == nil {
		t.Error("two rows on one day: accepted")
	}
}

func TestLoadTableRoundTrip(t *testing.T) {
	src := `{"prices":[
		{"provider":"acme","model":"m1","effective":"2026-11-01","currency":"USD",
		 "input":"1","output":"5","cacheRead":"0.10","source":"https://example.test/pricing, read 2026-11-01"}
	]}`
	tb, err := usage.LoadTable(strings.NewReader(src))
	if err != nil {
		t.Fatalf("LoadTable: %v", err)
	}
	p, ok := tb.Lookup("acme", "m1", day("2026-11-05"))
	if !ok || p.CacheRead == nil || p.CacheRead.String() != "0.1" || p.CacheWrite != nil {
		t.Fatalf("Lookup = %+v, %v", p, ok)
	}
	b, err := json.Marshal(map[string]any{"prices": []usage.Price{p}})
	if err != nil {
		t.Fatal(err)
	}
	again, err := usage.LoadTable(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("reload %s: %v", b, err)
	}
	if q, _ := again.Lookup("acme", "m1", day("2026-11-05")); q.ID() != p.ID() || q.Input != p.Input || *q.CacheRead != *p.CacheRead || q.Source != p.Source {
		t.Errorf("round trip changed the row: %+v != %+v", q, p)
	}
}

func TestLoadTableRefusesUnknownFieldsAndBadDates(t *testing.T) {
	for name, src := range map[string]string{
		"misspelt rate": `{"prices":[{"provider":"a","model":"m","effective":"2026-11-01","currency":"USD","input":"1","output":"1","cache_read":"0.1","source":"s"}]}`,
		"bad date":      `{"prices":[{"provider":"a","model":"m","effective":"1 Nov 2026","currency":"USD","input":"1","output":"1","source":"s"}]}`,
		"float rate":    `{"prices":[{"provider":"a","model":"m","effective":"2026-11-01","currency":"USD","input":1.5,"output":"1","source":"s"}]}`,
		"top level":     `{"rows":[]}`,
	} {
		if _, err := usage.LoadTable(strings.NewReader(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRecordJSONRoundTrip(t *testing.T) {
	r := call("m1", day("2026-11-02").Add(90*time.Minute), reported(1000, 200, 300, 0))
	r.ParentRun, r.Principal = "r0", "ops"
	r.Cost = table(t).Price(r)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back usage.Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.At.Equal(r.At) || back.Tokens != r.Tokens || back.Cost != r.Cost || back.ParentRun != "r0" {
		t.Errorf("round trip changed the record:\n%s\n%+v", b, back)
	}
	if !strings.Contains(string(b), `"amount":"0.00173"`) {
		t.Errorf("amount not written as a decimal string: %s", b)
	}
}

func TestRecordValidate(t *testing.T) {
	good := call("m1", day("2026-11-02"), reported(10, 10, 0, 0))
	if err := good.Validate(); err != nil {
		t.Fatalf("valid record refused: %v", err)
	}
	for name, mut := range map[string]func(*usage.Record){
		"no run":             func(r *usage.Record) { r.Run = "" },
		"no call":            func(r *usage.Record) { r.Call = "" },
		"no provider":        func(r *usage.Record) { r.Provider = "" },
		"no model":           func(r *usage.Record) { r.Model = "" },
		"no time":            func(r *usage.Record) { r.At = time.Time{} },
		"no basis":           func(r *usage.Record) { r.Tokens.Output.Basis = "" },
		"estimate, no means": func(r *usage.Record) { r.Tokens.Output.Basis = usage.Estimated },
		"negative":           func(r *usage.Record) { r.Tokens.Input.N = -1 },
		"cache over input":   func(r *usage.Record) { r.Tokens.CacheRead.N = 11 },
	} {
		r := good
		mut(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSum(t *testing.T) {
	tb := table(t)
	a := call("m1", day("2026-11-02"), reported(1_000_000, 0, 0, 0))
	a.Cost = tb.Price(a) // 1 USD
	b := call("m1", day("2026-10-02"), reported(0, 100_000, 0, 0))
	b.Cost = tb.Price(b) // 1.5 USD
	c := call("unpriced", day("2026-11-02"), reported(10, 10, 0, 0))
	c.Cost = tb.Price(c)
	d := call("m1", day("2026-11-02"), reported(0, 0, 0, 0))
	d.Tokens.Output = usage.Count{N: 7, Basis: usage.Estimated, Method: "guess"}
	d.Cost = usage.Cost{Known: true, Amount: 2, Currency: "EUR", Basis: usage.Estimated}

	s, err := usage.Sum([]usage.Record{a, b, c, d})
	if err != nil {
		t.Fatal(err)
	}
	if s.Calls != 4 || s.Unpriced != 1 || s.Complete() {
		t.Errorf("calls %d unpriced %d complete %v", s.Calls, s.Unpriced, s.Complete())
	}
	if s.Tokens.Input.N != 1_000_010 || s.Tokens.Input.Basis != usage.Derived {
		t.Errorf("input %+v; want 1000010 derived", s.Tokens.Input)
	}
	if s.Tokens.Output.N != 100_017 || s.Tokens.Output.Basis != usage.Estimated || s.Tokens.Output.Method == "" {
		t.Errorf("output %+v; want 100017 estimated with a method", s.Tokens.Output)
	}
	if s.Costs["USD"].String() != "2.5" || s.Costs["EUR"] != 2 || s.CostBasis != usage.Estimated {
		t.Errorf("costs %v basis %s", s.Costs, s.CostBasis)
	}
	if got := s.Currencies(); len(got) != 2 || got[0] != "EUR" || got[1] != "USD" {
		t.Errorf("currencies %v", got)
	}

	bad := a
	bad.Tokens.Input.Basis = ""
	if _, err := usage.Sum([]usage.Record{bad}); err == nil {
		t.Error("Sum accepted a figure with no basis")
	}
	huge := a
	huge.Cost.Amount = 1<<63 - 1
	if _, err := usage.Sum([]usage.Record{huge, a}); err == nil {
		t.Error("Sum accepted an overflowing total")
	}
	if empty, err := usage.Sum(nil); err != nil || empty.Calls != 0 || !empty.Complete() {
		t.Errorf("empty sum: %+v %v", empty, err)
	}
}
