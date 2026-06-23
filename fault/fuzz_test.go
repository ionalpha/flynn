package fault_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ionalpha/flynn/fault"
)

func classAt(i int) fault.Class {
	n := len(allClasses)
	return allClasses[((i%n)+n)%n]
}

// FuzzErrorModel throws arbitrary strings at the error model — codes and
// messages that look like format specifiers, contain NULs, newlines, or
// non-ASCII — and asserts the invariants never break: Error() never panics,
// classification round-trips (directly and through an outer wrap), and a
// wrapped cause stays reachable via errors.Is.
func FuzzErrorModel(f *testing.F) {
	for _, s := range []struct {
		code, msg string
		cls       int
	}{
		{"", "", 0},
		{"net_timeout", "boom", 1},
		{"%s", "%d %v %w", 2},
		{"emoji_🔥", "naïve\nmultiline", 3},
		{"  spaces  ", "\t\x00ctrl", 4},
	} {
		f.Add(s.code, s.msg, s.cls)
	}

	f.Fuzz(func(t *testing.T, code, msg string, clsIdx int) {
		class := classAt(clsIdx)

		e := fault.New(class, code, msg)
		_ = e.Error() // must not panic on any input

		if got := fault.Classify(e); got != class {
			t.Fatalf("Classify(New) = %q, want %q", got, class)
		}

		cause := errors.New(code)
		w := fault.Wrap(class, code, cause)
		_ = w.Error()
		if !errors.Is(w, cause) {
			t.Fatal("Wrap lost its cause")
		}
		if got := fault.Classify(fmt.Errorf("ctx: %w", w)); got != class {
			t.Fatalf("Classify(nested) = %q, want %q", got, class)
		}
	})
}
