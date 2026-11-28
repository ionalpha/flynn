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

func TestInert(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"\x1b[31mred\x1b[0m", "[31mred[0m"},
		{"title\x1b]0;pwned\x07", "title]0;pwned"},
		{"bell\x07 and del\x7f", "bell and del"},
		{"c1 \u009b2J csi", "c1 2J csi"},
		{"keeps\nnewlines\r\n", "keeps\nnewlines\n"},
		{"héllo 日本", "héllo 日本"},
	}
	for _, tc := range cases {
		if got := Inert(tc.in); got != tc.want {
			t.Errorf("Inert(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestInertExpandsTabsToStops: a tab reaches the next eight-column stop, counted in
// display cells after controls are stripped and restarted on every line.
func TestInertExpandsTabsToStops(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"leading tab", "\tindented", "        indented"},
		{"mid word", "tab\there", "tab     here"},
		{"exact stop", "12345678\tx", "12345678        x"},
		{"two tabs", "\t\tdeep", "                deep"},
		{"shell output", "total 8\n\tmain.go\n\t\tlines\t42", "total 8\n        main.go\n                lines   42"},
		{"goal final answer", "Done.\nfile\tstatus\nmain.go\tfixed", "Done.\nfile    status\nmain.go fixed"},
		{"wide runes", "日本\tx", "日本    x"},
		{"after stripped escape", "\x1b[1mab\tc", "[1mab   c"},
		{"after stripped bell", "a\x07b\tc", "ab      c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Inert(tc.in); got != tc.want {
				t.Errorf("Inert(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
