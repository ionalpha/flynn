package watch

import "testing"

func TestScanLine(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantOK   bool
		wantKind Kind
		wantText string
		wantCode string
	}{
		{name: "go act", line: "x := 1 // ai! rename to count", wantOK: true, wantKind: Act, wantText: "rename to count", wantCode: "x := 1"},
		{name: "go ask", line: "\tfoo() // ai? what does this return", wantOK: true, wantKind: Ask, wantText: "what does this return", wantCode: "\tfoo()"},
		{name: "hash comment", line: "value = 3  # ai! make it configurable", wantOK: true, wantKind: Act, wantText: "make it configurable", wantCode: "value = 3"},
		{name: "block comment", line: "int y; /* ai! widen to int64 */", wantOK: true, wantKind: Act, wantText: "widen to int64", wantCode: "int y;"},
		{name: "html comment", line: "<h1>Hi</h1> <!-- ai! add a subtitle -->", wantOK: true, wantKind: Act, wantText: "add a subtitle", wantCode: "<h1>Hi</h1>"},
		{name: "sql dash", line: "SELECT 1 -- ai? is this indexed", wantOK: true, wantKind: Ask, wantText: "is this indexed", wantCode: "SELECT 1"},
		{name: "marker only", line: "// ai! write the docstring", wantOK: true, wantKind: Act, wantText: "write the docstring", wantCode: ""},
		{name: "no marker", line: "x := 1 // just a normal comment", wantOK: false},
		{name: "bare ai not marker", line: "airplane := true", wantOK: false},
		{name: "ai without punctuation", line: "// ai do something", wantOK: false},
		{name: "empty instruction", line: "x := 1 // ai!", wantOK: false},
		{name: "empty instruction spaces", line: "x := 1 // ai!   ", wantOK: false},
		{name: "glued bang", line: "// ai!! yikes", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, text, code, ok := ScanLine(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

func TestScanMultiline(t *testing.T) {
	content := []byte("package main\n\nfunc main() {} // ai! add logging\nvar x = 2 // plain\n// ai? why global\n")
	got := Scan("main.go", content)
	if len(got) != 2 {
		t.Fatalf("got %d markers, want 2: %+v", len(got), got)
	}
	if got[0].Line != 3 || got[0].Kind != Act || got[0].Text != "add logging" {
		t.Errorf("marker 0 = %+v", got[0])
	}
	if got[1].Line != 5 || got[1].Kind != Ask || got[1].Text != "why global" {
		t.Errorf("marker 1 = %+v", got[1])
	}
	if got[0].File != "main.go" {
		t.Errorf("file = %q, want main.go", got[0].File)
	}
}

func TestProvenance(t *testing.T) {
	m := Marker{Kind: Act, File: "cmd/flynn/run.go", Line: 42}
	if got, want := m.Provenance(), "cmd/flynn/run.go:42 (ai!)"; got != want {
		t.Errorf("Provenance() = %q, want %q", got, want)
	}
}
