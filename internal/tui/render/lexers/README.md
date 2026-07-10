# Embedded lexer definitions

Serialised Chroma lexers, generated from the `github.com/alecthomas/chroma/v2`
version pinned in `go.mod` and used under the MIT license in `LICENSE.chroma`.

They are checked in rather than imported because `chroma/v2/lexers` parses all
279 of its definitions in its package init, and Go runs the init of every
package linked into a binary. Importing it anywhere cost every `flynn` process
5.2ms and 2.7MB, including `flynn --version`, `flynn mcp serve`, and headless CI
runs that never render a code block. Here the definitions parse on the first
code block instead, and only for the languages a session meets.

## Regenerating

After a chroma upgrade:

    go test ./internal/tui/render -run Lexers -update

Then rerun without `-update` and read the diff: `TestEmbeddedLexersMatchChroma`
fails whenever these files are not exactly what the pinned chroma produces.

To add a language, add one line to `embeddedLexers` in `lexers_test.go` and
regenerate. A language with no definition here highlights as plain code.

## The one divergence

Chroma tokenizes a Go raw string by handing its body to the text/template
lexer, which is a Go function and cannot be expressed in XML. `go.xml` emits
the whole backquoted literal as a string, so `{{.X}}` inside a raw string is
not highlighted as template syntax. `TestGoRawStringDoesNotTemplate` pins that.
