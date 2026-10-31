package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeOnceThenDone is the scripted model for the provider parity check. On the first
// build turn it calls the write tool in whichever form the request asked for: a request
// carrying a decode-time grammar (llamacpp) gets the call as the JSON the grammar admits
// as content, and any other request gets a tool_calls reply. It then finishes.
func writeOnceThenDone(req oaiRequest, n int) oaiReply {
	if n > 0 {
		return finalText("Wrote answer.txt.")
	}
	if req.Grammar != "" {
		return finalText(`{"name":"write","arguments":{"path":"answer.txt","content":"42\n"}}`)
	}
	return toolCall("call_1", "write", `{"path":"answer.txt","content":"42\n"}`)
}

// TestEveryProviderNameGetsTheSameToolsOnTheBinary is the compiled-binary form of the
// package-level parity test in cmd/flynn (provider_grant_test.go). One fake server is
// reached under each built-in provider name, with that name's base-URL variable pointed
// at it, and the goal must run its tool every time. A request reporting capability_denied
// would mean the grant depended on the provider name rather than the run's toolset.
func TestEveryProviderNameGetsTheSameToolsOnTheBinary(t *testing.T) {
	cases := []struct {
		provider string
		baseEnv  string
		grammar  bool // llamacpp is the one kind that sends its tools as a grammar
	}{
		{"openai", "OPENAI_BASE_URL", false},
		{"llamacpp", "LLAMACPP_BASE_URL", true},
		{"ollama", "OLLAMA_BASE_URL", false},
		{"lmstudio", "LMSTUDIO_BASE_URL", false},
	}
	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			fake := newFakeOpenAIFunc(t, writeOnceThenDone)
			in := newInstance(t)
			in.model = "" // the goal names its model with --model, as an operator would
			if c.provider == "openai" {
				in.withModel(fake)
			} else {
				in.setEnv(c.baseEnv, fake.baseURL())
			}

			res := in.run("-no-learn", "goal", "--model", c.provider+":m", "write 42 to answer.txt")
			requireExit(t, res, 0, "goal under "+c.provider)

			got, err := in.workfile("answer.txt")
			if err != nil {
				t.Fatalf("the write tool did not run under %s: %v", c.provider, err)
			}
			if string(got) != "42\n" {
				t.Fatalf("artifact under %s: expected %q, got %q", c.provider, "42\n", string(got))
			}

			if fake.count() < 2 {
				t.Fatalf("expected a tool turn and a fold under %s, got %d model call(s)", c.provider, fake.count())
			}
			first := fake.request(t, 0)
			if first.Model != "m" {
				t.Fatalf("the request under %s named model %q, want %q", c.provider, first.Model, "m")
			}
			if c.grammar {
				if first.Grammar == "" {
					t.Fatalf("%s sent no grammar; the grammar path this case exists to cover did not run", c.provider)
				}
			} else if !slices.Contains(first.Tools, "write") {
				t.Fatalf("%s advertised tools %v; want the write tool in the tools field", c.provider, first.Tools)
			}

			second := fake.request(t, 1)
			if !slices.ContainsFunc(second.Messages, func(m oaiMessage) bool { return m.Role == "tool" }) {
				t.Fatalf("the second request under %s carried no tool result; roles were %v", c.provider, roles(second))
			}

			for i := range fake.count() {
				body, err := json.Marshal(fake.request(t, i).Raw)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(body), "capability_denied") {
					t.Fatalf("request %d under %s reports a denied capability:\n%s", i, c.provider, body)
				}
			}
		})
	}
}

// TestProvidersFileNamesAServerTheGoalReaches writes a named server into providers.json in
// the scenario's data directory and runs a goal against it as --model box:m. The request
// must reach the fake under the model name after the colon, which proves the binary reads
// the file from the data directory and dials the base_url it names.
func TestProvidersFileNamesAServerTheGoalReaches(t *testing.T) {
	fake := newFakeOpenAIFunc(t, writeOnceThenDone)
	in := newInstance(t)
	in.model = ""
	body, err := json.Marshal(map[string]any{
		"providers": map[string]any{
			"box": map[string]any{"kind": "ollama", "base_url": fake.baseURL()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in.dataDir, "providers.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	res := in.run("-no-learn", "goal", "--model", "box:m", "write 42 to answer.txt")
	requireExit(t, res, 0, "goal against a providers.json server")

	if fake.count() == 0 {
		t.Fatalf("the goal never reached the providers.json server:\n%s", res.stderr)
	}
	if got := fake.request(t, 0).Model; got != "m" {
		t.Fatalf("the request named model %q, want %q", got, "m")
	}
}

// TestProvidersFileMissingNameIsRefused is the other side of the same file: with no
// providers.json, box is not a provider, so the goal exits non-zero before any request.
func TestProvidersFileMissingNameIsRefused(t *testing.T) {
	fake := newFakeOpenAIFunc(t, writeOnceThenDone)
	in := newInstance(t)
	in.model = ""

	res := in.run("-no-learn", "goal", "--model", "box:m", "write 42 to answer.txt")
	requireExit(t, res, 1, "goal against an unnamed provider")
	requireContains(t, res.combined(), "unknown provider", "output for an unnamed provider")
	if fake.count() != 0 {
		t.Fatalf("an unnamed provider still reached the server (%d request(s))", fake.count())
	}
}
