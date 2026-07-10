package render

import (
	"bytes"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// updateLexers regenerates the embedded lexer definitions from the chroma
// version in go.mod. Run `go test ./internal/tui/render -run Lexers -update`
// after a chroma upgrade and commit the diff.
var updateLexers = flag.Bool("update", false, "regenerate the embedded lexer definitions from chroma")

// embeddedLexers maps each embedded file to the chroma lexer it is generated
// from. The set is the languages a session realistically meets in model
// output; a language absent here highlights as plain code, which is what an
// unknown fence tag already does. Adding one is a one-line change plus
// `-update`.
var embeddedLexers = map[string]string{
	"bash.xml":       "bash",
	"batchfile.xml":  "batch",
	"c.xml":          "c",
	"clojure.xml":    "clojure",
	"cmake.xml":      "cmake",
	"cpp.xml":        "c++",
	"csharp.xml":     "c#",
	"css.xml":        "css",
	"dart.xml":       "dart",
	"diff.xml":       "diff",
	"docker.xml":     "docker",
	"elixir.xml":     "elixir",
	"erlang.xml":     "erlang",
	"fish.xml":       "fish",
	"go.xml":         "go",
	"graphql.xml":    "graphql",
	"groovy.xml":     "groovy",
	"haskell.xml":    "haskell",
	"html.xml":       "html",
	"ini.xml":        "ini",
	"java.xml":       "java",
	"javascript.xml": "javascript",
	"json.xml":       "json",
	"julia.xml":      "julia",
	"kotlin.xml":     "kotlin",
	"lua.xml":        "lua",
	"makefile.xml":   "make",
	"nix.xml":        "nix",
	"ocaml.xml":      "ocaml",
	"perl.xml":       "perl",
	"php.xml":        "php",
	"plaintext.xml":  "plaintext",
	"powershell.xml": "powershell",
	"protobuf.xml":   "protobuf",
	"python.xml":     "python",
	"r.xml":          "r",
	"ruby.xml":       "ruby",
	"rust.xml":       "rust",
	"scala.xml":      "scala",
	"sql.xml":        "sql",
	"swift.xml":      "swift",
	"terraform.xml":  "terraform",
	"toml.xml":       "toml",
	"typescript.xml": "typescript",
	"xml.xml":        "xml",
	"yaml.xml":       "yaml",
	"zig.xml":        "zig",
}

// goRawString is the one rule that does not survive serialisation. chroma
// tokenizes a Go raw string by handing its body to the text/template lexer,
// which is a Go function and not expressible in XML. The embedded definition
// emits the whole backquoted literal as a string instead, so `{{.X}}` inside
// a raw string renders as string rather than as template syntax. Nothing else
// about Go highlighting changes; TestGoRawStringDoesNotTemplate pins it.
var goRawString = chroma.Rule{Pattern: "`[^`]*`", Type: chroma.LiteralString}

// goRawStringPattern is the upstream rule goRawString replaces. If chroma
// rewrites it, generation fails loudly rather than silently emitting a lexer
// that cannot round-trip.
const goRawStringPattern = "(`)([^`]*)(`)"

// marshalUpstream serialises the named chroma lexer to canonical XML.
func marshalUpstream(t *testing.T, name string) []byte {
	t.Helper()
	lex := lexers.Get(name)
	if lex == nil {
		t.Fatalf("chroma has no lexer %q", name)
	}
	rl, ok := lex.(*chroma.RegexLexer)
	if !ok {
		t.Fatalf("lexer %q is a %T, not a RegexLexer, so it cannot be serialised", name, lex)
	}
	rules, err := rl.Rules()
	if err != nil {
		t.Fatalf("rules for %q: %v", name, err)
	}
	if name == "go" {
		replaceGoRawString(t, rules)
	}
	data, err := chroma.Marshal(chroma.MustNewLexer(rl.Config(), func() chroma.Rules { return rules }))
	if err != nil {
		t.Fatalf("marshal %q: %v", name, err)
	}
	return canonicalLexerXML(data)
}

// replaceGoRawString swaps the one unserialisable rule in the Go lexer.
func replaceGoRawString(t *testing.T, rules chroma.Rules) {
	t.Helper()
	found := false
	for state, rs := range rules {
		for i, r := range rs {
			if r.Pattern == goRawStringPattern {
				rules[state][i] = goRawString
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("chroma's Go lexer no longer has the raw-string rule %q; re-check what is unserialisable before changing this", goRawStringPattern)
	}
}

// canonicalLexerXML sorts the <state> elements so a regenerated definition is
// byte-identical run to run. chroma marshals its rules from a map, so the
// state order it emits is whatever the runtime hands it.
func canonicalLexerXML(data []byte) []byte {
	const sep = "\n    <state "
	head, rest, ok := strings.Cut(string(data), sep)
	if !ok {
		return append(bytes.TrimRight(data, "\n"), '\n')
	}
	tail := ""
	states := strings.Split(rest, sep)
	if last := len(states) - 1; last >= 0 {
		if end := strings.Index(states[last], "\n    </state>"); end >= 0 {
			cut := end + len("\n    </state>")
			tail = states[last][cut:]
			states[last] = states[last][:cut]
		}
	}
	sort.Strings(states)
	var b strings.Builder
	b.WriteString(head)
	for _, s := range states {
		b.WriteString(sep)
		b.WriteString(s)
	}
	b.WriteString(tail)
	return append(bytes.TrimRight([]byte(b.String()), "\n"), '\n')
}

// TestEmbeddedLexersMatchChroma is the drift gate: every embedded definition
// is exactly what the pinned chroma produces, and the directory holds nothing
// else. A chroma upgrade that changes a lexer turns this red until someone
// reruns it with -update and looks at the diff.
func TestEmbeddedLexersMatchChroma(t *testing.T) {
	for file, name := range embeddedLexers {
		want := marshalUpstream(t, name)
		path := filepath.Join(lexerDir, file)
		if *updateLexers {
			if err := os.WriteFile(path, want, 0o600); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (rerun with -update): %v", path, err)
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
			t.Errorf("%s is stale; rerun with -update", path)
		}
	}
	if *updateLexers {
		t.Log("regenerated; rerun without -update to verify")
		return
	}
	entries, err := os.ReadDir(lexerDir)
	if err != nil {
		t.Fatalf("read %s: %v", lexerDir, err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".xml") {
			continue // the license and the README are not lexers
		}
		if _, ok := embeddedLexers[e.Name()]; !ok {
			t.Errorf("%s is not generated by any entry in embeddedLexers; delete it or claim it", e.Name())
		}
	}
}

// TestEveryEmbeddedLexerRegisters proves the lazy registry reaches every
// embedded definition: none is skipped by the parse guard in lexerRegistry,
// and each is reachable by the name a fence info tag would carry.
func TestEveryEmbeddedLexerRegisters(t *testing.T) {
	entries, err := fs.ReadDir(lexerFS, lexerDir)
	if err != nil {
		t.Fatalf("read embedded %s: %v", lexerDir, err)
	}
	if len(entries) != len(embeddedLexers) {
		t.Fatalf("embedded %d definitions, table names %d", len(entries), len(embeddedLexers))
	}
	for _, e := range entries {
		if _, err := chroma.NewXMLLexer(lexerFS, lexerDir+"/"+e.Name()); err != nil {
			t.Errorf("%s does not parse, so the registry would silently skip it: %v", e.Name(), err)
		}
	}
	reg := lexerRegistry()
	if got := len(reg.Lexers); got != len(embeddedLexers) {
		t.Fatalf("registry holds %d lexers, want %d", got, len(embeddedLexers))
	}
	for _, name := range embeddedLexers {
		if reg.Get(name) == nil {
			t.Errorf("registry cannot resolve %q", name)
		}
	}
}

// TestGoRawStringDoesNotTemplate pins the single divergence from upstream
// chroma, so it stays a decision rather than a surprise.
func TestGoRawStringDoesNotTemplate(t *testing.T) {
	lex := lexerRegistry().Get("go")
	if lex == nil {
		t.Fatal("no Go lexer")
	}
	it, err := lex.Tokenise(nil, "var t = `{{.Name}}`\n")
	if err != nil {
		t.Fatalf("tokenise: %v", err)
	}
	var raw string
	for tok := it(); tok != chroma.EOF; tok = it() {
		if strings.HasPrefix(tok.Value, "`") {
			raw = tok.Value
			if !tok.Type.InSubCategory(chroma.LiteralString) {
				t.Errorf("raw string tokenized as %v, want a string type", tok.Type)
			}
		}
	}
	if raw != "`{{.Name}}`" {
		t.Errorf("raw string emitted as %q, want the whole literal in one token", raw)
	}
}
