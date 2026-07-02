package flow_test

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/internal/flow"
)

// The hot path under benchmark: one decoded flow evaluated over many elements.
// Decode compiles every expression and template once, so the per-element cost is
// pure evaluation; these benchmarks are the regression gate for that guarantee.

func benchItems(n int) map[string]any {
	items := make([]any, n)
	for i := range items {
		items[i] = float64(i)
	}
	return map[string]any{"items": items}
}

func decodeBenchFlow(b *testing.B, src string) flow.Flow {
	b.Helper()
	f, err := flow.Decode([]byte(src))
	if err != nil {
		b.Fatal(err)
	}
	return f
}

// BenchmarkTransformMap maps an expression over 1000 elements: the per-element
// cost of an already-compiled transform.
func BenchmarkTransformMap(b *testing.B) {
	f := decodeBenchFlow(b, `{"steps":[
		{"id":"m","op":"transform","transform":{"source":"config.items","map":"it + 1"}},
		{"op":"return","return":{"value":"{{steps.m}}"}}
	]}`)
	in := flow.New()
	config := benchItems(1000)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := in.Run(ctx, f, config)
		if err != nil {
			b.Fatal(err)
		}
		if len(out.([]any)) != 1000 {
			b.Fatalf("got %d elements", len(out.([]any)))
		}
	}
}

// BenchmarkLoopCollect runs a collect-only loop over 1000 elements: the
// per-iteration cost of an already-compiled loop.
func BenchmarkLoopCollect(b *testing.B) {
	f := decodeBenchFlow(b, `{"steps":[
		{"id":"l","op":"loop","loop":{"over":"config.items","collect":"item + 1"}},
		{"op":"return","return":{"value":"{{steps.l}}"}}
	]}`)
	in := flow.New()
	config := benchItems(1000)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := in.Run(ctx, f, config)
		if err != nil {
			b.Fatal(err)
		}
		if len(out.([]any)) != 1000 {
			b.Fatalf("got %d elements", len(out.([]any)))
		}
	}
}

// BenchmarkDecode prices the compile itself, so making execution cheap cannot
// silently make admission expensive.
func BenchmarkDecode(b *testing.B) {
	src := []byte(`{"steps":[
		{"id":"m","op":"transform","transform":{"source":"config.items","map":"it + 1"}},
		{"op":"condition","condition":{"if":"steps.m","then":[{"op":"assert","assert":{"that":"steps.m"}}]}},
		{"op":"return","return":{"value":{"out":"{{steps.m}}","label":"n={{config.n}}"}}}
	]}`)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := flow.Decode(src); err != nil {
			b.Fatal(err)
		}
	}
}
