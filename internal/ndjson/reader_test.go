package ndjson

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

// outcome is one reader result reduced to what a test compares: a kept line or a refusal.
type outcome struct {
	line string
	kind Kind
}

// drain reads r to the end and returns every outcome in order.
func drain(t *testing.T, r io.Reader) []outcome {
	t.Helper()
	rd := NewReader(r)
	var out []outcome
	for {
		line, err := rd.Next()
		var fe *FrameError
		switch {
		case err == nil:
			out = append(out, outcome{line: string(line)})
		case errors.As(err, &fe):
			out = append(out, outcome{kind: fe.Kind})
		case errors.Is(err, io.EOF):
			return out
		default:
			t.Fatalf("Next: %v", err)
		}
	}
}

func TestReaderSplitsOnLFOnly(t *testing.T) {
	// U+2028, U+2029, a bare CR and a CRLF tail inside or around a record are data. Only
	// LF ends a record.
	in := "{\"a\":\"x\u2028y\u2029z\rw\"}\n{\"b\":1}\r\n"
	got := drain(t, strings.NewReader(in))
	want := []outcome{{line: "{\"a\":\"x\u2028y\u2029z\rw\"}"}, {line: "{\"b\":1}\r"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReaderRefusals(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []outcome
	}{
		{"batch", "[{\"a\":1}]\n{\"a\":2}\n", []outcome{{kind: KindBatch}, {line: "{\"a\":2}"}}},
		{"scalar", "7\n{}\n", []outcome{{kind: KindNotObject}, {line: "{}"}}},
		{"string", "\"x\"\n{}\n", []outcome{{kind: KindNotObject}, {line: "{}"}}},
		{"invalid utf8", "{\"a\":\"\xff\"}\n{}\n", []outcome{{kind: KindEncoding}, {line: "{}"}}},
		{"raw surrogate", "{\"a\":\"\xed\xa0\x80\"}\n{}\n", []outcome{{kind: KindEncoding}, {line: "{}"}}},
		{"escaped lone surrogate is text", "{\"a\":\"\\ud800\"}\n", []outcome{{line: "{\"a\":\"\\ud800\"}"}}},
		{"blank lines skipped", "\n \t\r\n{}\n\n", []outcome{{line: "{}"}}},
		{"unterminated tail", "{}\n{\"a\":", []outcome{{line: "{}"}, {kind: KindUnterminated}}},
		{"depth at limit", strings.Repeat("[", MaxDepth-1) + "{}" + strings.Repeat("]", MaxDepth-1), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == nil {
				return
			}
			got := drain(t, strings.NewReader(tc.in))
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("outcome %d: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestReaderDepthBoundary(t *testing.T) {
	nest := func(n int) string {
		// An object whose value is n-1 further arrays: n containers in all.
		return "{\"a\":" + strings.Repeat("[", n-1) + strings.Repeat("]", n-1) + "}\n"
	}
	if got := drain(t, strings.NewReader(nest(MaxDepth))); len(got) != 1 || got[0].kind != 0 {
		t.Fatalf("depth %d was refused: %v", MaxDepth, got)
	}
	got := drain(t, strings.NewReader(nest(MaxDepth+1)+"{}\n"))
	if len(got) != 2 || got[0].kind != KindDepth || got[1].line != "{}" {
		t.Fatalf("depth %d: got %v", MaxDepth+1, got)
	}
}

func TestReaderBracketsInStringsAreNotNesting(t *testing.T) {
	in := "{\"a\":\"" + strings.Repeat("[", 10*MaxDepth) + "\\\"" + strings.Repeat("{", 10*MaxDepth) + "\"}\n"
	if got := drain(t, strings.NewReader(in)); len(got) != 1 || got[0].kind != 0 {
		t.Fatalf("brackets inside a string counted as nesting: %.60v", got)
	}
}

func TestReaderDeepNestingDoesNotRecurse(t *testing.T) {
	// A megabyte of opening brackets would overflow the stack of a recursive checker.
	in := "{\"a\":" + strings.Repeat("[", MaxLine-8) + "\n{}\n"
	got := drain(t, strings.NewReader(in))
	if len(got) != 2 || got[0].kind != KindDepth || got[1].line != "{}" {
		t.Fatalf("got %v", got)
	}
}

func TestReaderLineSizeBoundary(t *testing.T) {
	pad := func(n int) string { // an object of exactly n bytes
		return "{\"a\":\"" + strings.Repeat("x", n-8) + "\"}"
	}
	if got := drain(t, strings.NewReader(pad(MaxLine)+"\n")); len(got) != 1 || got[0].kind != 0 {
		t.Fatalf("a line of exactly MaxLine bytes was refused")
	}
	got := drain(t, strings.NewReader(pad(MaxLine+1)+"\n{}\n"))
	if len(got) != 2 || got[0].kind != KindOversize || got[1].line != "{}" {
		t.Fatalf("a line of MaxLine+1 bytes: got %v", got)
	}
}

// endless yields n bytes of 'a' and then the tail, without ever holding the n bytes.
type endless struct {
	n    int64
	tail []byte
}

func (e *endless) Read(p []byte) (int, error) {
	if e.n > 0 {
		k := int64(len(p))
		if k > e.n {
			k = e.n
		}
		for i := range p[:k] {
			p[i] = 'a'
		}
		e.n -= k
		return int(k), nil
	}
	if len(e.tail) == 0 {
		return 0, io.EOF
	}
	n := copy(p, e.tail)
	e.tail = e.tail[n:]
	return n, nil
}

// TestOversizeLineAllocatesBoundedMemory counts bytes allocated, not elapsed time, while
// the reader discards a 100 MiB line and resynchronises on the record after it.
func TestOversizeLineAllocatesBoundedMemory(t *testing.T) {
	const huge = 100 << 20
	rd := NewReader(&endless{n: huge, tail: []byte("\n{\"after\":true}\n")})

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := rd.Next()
	runtime.ReadMemStats(&after)

	var fe *FrameError
	if !errors.As(err, &fe) || fe.Kind != KindOversize {
		t.Fatalf("Next error = %v, want an oversize refusal", err)
	}
	if fe.Size != huge {
		t.Errorf("reported size %d, want %d", fe.Size, huge)
	}
	// The line buffer grows by append up to the cap, which allocates several times the cap
	// in total as it is outgrown, plus the fixed read buffer. None of that depends on the
	// line's length; buffering the line itself would be 100 MiB.
	const budget = 8 * MaxLine
	if grown := after.TotalAlloc - before.TotalAlloc; grown > budget {
		t.Fatalf("discarding a %d byte line allocated %d bytes, budget %d", huge, grown, budget)
	}
	line, err := rd.Next()
	if err != nil || string(line) != "{\"after\":true}" {
		t.Fatalf("after the oversize line: %q, %v", line, err)
	}
}

func TestReaderPropagatesReadError(t *testing.T) {
	boom := errors.New("boom")
	rd := NewReader(io.MultiReader(strings.NewReader("{}\n{\"a\""), iotest.ErrReader(boom)))
	if _, err := rd.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Next(); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the reader's error", err)
	}
}

// reference classifies input without the reader's streaming logic: split on LF, judge
// each segment on its own. The reader must agree with it however the bytes arrive.
func reference(in []byte) []outcome {
	var out []outcome
	segs := bytes.Split(in, []byte{'\n'})
	for i, seg := range segs {
		last := i == len(segs)-1
		if last && len(seg) == 0 {
			break
		}
		switch {
		case len(seg) > MaxLine:
			out = append(out, outcome{kind: KindOversize})
		case last:
			out = append(out, outcome{kind: KindUnterminated})
		case strings.Trim(string(seg), " \t\r") == "":
		case !utf8.Valid(seg):
			out = append(out, outcome{kind: KindEncoding})
		default:
			trimmed := strings.TrimLeft(string(seg), " \t\r")
			switch {
			case trimmed[0] == '[':
				out = append(out, outcome{kind: KindBatch})
			case trimmed[0] != '{':
				out = append(out, outcome{kind: KindNotObject})
			case referenceDepth(seg) > MaxDepth:
				out = append(out, outcome{kind: KindDepth})
			default:
				out = append(out, outcome{line: string(seg)})
			}
		}
	}
	return out
}

// referenceDepth is the deepest nesting reached, computed by a different route from the
// reader's early-exit scan: it tracks the maximum over the whole line.
func referenceDepth(seg []byte) int {
	depth, deepest := 0, 0
	esc, str := false, false
	for _, c := range seg {
		switch {
		case str && esc:
			esc = false
		case str && c == '\\':
			esc = true
		case str && c == '"':
			str = false
		case str:
		case c == '"':
			str = true
		case c == '{' || c == '[':
			depth++
			deepest = max(deepest, depth)
		case (c == '}' || c == ']') && depth > 0:
			depth--
		}
	}
	return deepest
}

func FuzzReaderResynchronises(f *testing.F) {
	seeds := []string{
		"{}\n", "{\"a\":1}\n{\"b\":2}\n", "[1]\n{}\n", "{\"a\":\"\u2028\u2029\"}\n{}\n",
		"{\"a\":\"\\ud800\\udc00\"}\n", "{\"a\":\"\xed\xa0\x80\"}\n{}\n", "\xff\xfe\n{}\n",
		"{\"a\":" + strings.Repeat("[", 80) + "\n{}\n", "{\"a\":\"\\\"[[[\"}\n", "{", "\n\n\n", "{}\n{",
		strings.Repeat("x", MaxLine+3) + "\n{}\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s), uint8(7))
	}
	f.Fuzz(func(t *testing.T, in []byte, chunk uint8) {
		want := reference(in)
		// One byte at a time, a fuzz-chosen chunk size and all at once must all agree.
		sources := map[string]io.Reader{
			"all":    bytes.NewReader(in),
			"one":    iotest.OneByteReader(bytes.NewReader(in)),
			"chunks": &chunked{b: in, n: int(chunk) + 1},
		}
		for name, src := range sources {
			got := drain(t, src)
			if len(got) != len(want) {
				t.Fatalf("%s: %d outcomes, reference has %d\ngot  %+v\nwant %+v", name, len(got), len(want), got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s: outcome %d = %+v, reference %+v", name, i, got[i], want[i])
				}
				if len(got[i].line) > MaxLine || strings.Contains(got[i].line, "\n") {
					t.Fatalf("%s: kept line breaks a cap: %d bytes", name, len(got[i].line))
				}
			}
		}
	})
}

// chunked hands out its bytes n at a time.
type chunked struct {
	b []byte
	n int
}

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	k := min(c.n, len(p), len(c.b))
	copy(p, c.b[:k])
	c.b = c.b[k:]
	return k, nil
}
