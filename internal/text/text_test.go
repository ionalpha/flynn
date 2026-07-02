package text

import "testing"

func TestClip(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-10", 10, "exactly-10"},
		{"this is longer", 7, "this is..."},
		{"trailing space  cut", 15, "trailing space..."},
		{"héllo wörld", 5, "héllo..."},
		{"日本語のテキスト", 3, "日本語..."},
		{"", 5, ""},
	}
	for _, tc := range cases {
		if got := Clip(tc.in, tc.n); got != tc.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestOneLine(t *testing.T) {
	if got := OneLine("a\n\n b\t c  d", 100); got != "a b c d" {
		t.Fatalf("OneLine = %q", got)
	}
	if got := OneLine("one two three", 7); got != "one two..." {
		t.Fatalf("OneLine clip = %q", got)
	}
}

func TestContainsAny(t *testing.T) {
	if !ContainsAny("out of memory on device", "quota", "memory") {
		t.Fatal("must match the second substring")
	}
	if ContainsAny("all fine", "quota", "memory") {
		t.Fatal("must not match")
	}
	if ContainsAny("anything") {
		t.Fatal("no substrings never matches")
	}
}
