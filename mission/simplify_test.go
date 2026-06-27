package mission

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestSimplifyStripsDocsKeepsSurface proves the core safety property: simplification removes
// documentation noise (descriptions, examples, titles) at every level but preserves the callable
// surface, every property, the required set, and meaning-bearing keywords (enum, default).
func TestSimplifyStripsDocsKeepsSurface(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"description": "a verbose object",
		"required": ["path"],
		"properties": {
			"path": {"type": "string", "description": "where to look", "examples": ["/a", "/b"]},
			"mode": {"type": "string", "enum": ["r", "w"], "default": "r", "title": "Mode"}
		}
	}`)
	out := simplifyTool(llm.Tool{Name: "read", Description: "read a file", InputSchema: schema})

	var got map[string]any
	if err := json.Unmarshal(out.InputSchema, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["description"]; ok {
		t.Fatal("top-level description survived simplification")
	}
	req, _ := got["required"].([]any)
	if len(req) != 1 || req[0] != "path" {
		t.Fatalf("required set not preserved: %v", got["required"])
	}
	props, _ := got["properties"].(map[string]any)
	if len(props) != 2 {
		t.Fatalf("properties dropped: %v", props)
	}
	path, _ := props["path"].(map[string]any)
	if _, ok := path["description"]; ok {
		t.Fatal("nested property description survived")
	}
	if _, ok := path["examples"]; ok {
		t.Fatal("examples survived")
	}
	if path["type"] != "string" {
		t.Fatalf("property type lost: %v", path)
	}
	mode, _ := props["mode"].(map[string]any)
	if _, ok := mode["title"]; ok {
		t.Fatal("title survived")
	}
	enum, _ := mode["enum"].([]any)
	if len(enum) != 2 {
		t.Fatalf("enum (a real constraint) was dropped: %v", mode)
	}
	if mode["default"] != "r" {
		t.Fatalf("default (meaning-bearing) was dropped: %v", mode)
	}
}

// TestSimplifyTrimsDescription proves a long tool description is shortened, cutting at a sentence
// boundary so it stays readable, while a short one is left as is.
func TestSimplifyTrimsDescription(t *testing.T) {
	long := "Reads a file from the working directory. " + strings.Repeat("Extra detail. ", 30)
	out := simplifyTool(llm.Tool{Name: "read", Description: long, InputSchema: json.RawMessage(`{}`)})
	if len(out.Description) > maxToolDescription {
		t.Fatalf("description not trimmed: %d chars", len(out.Description))
	}
	if !strings.HasPrefix(out.Description, "Reads a file") {
		t.Fatalf("trim lost the opening sentence: %q", out.Description)
	}

	short := "echo the input"
	out2 := simplifyTool(llm.Tool{Name: "echo", Description: short, InputSchema: json.RawMessage(`{}`)})
	if out2.Description != short {
		t.Fatalf("short description altered: %q", out2.Description)
	}
}

// TestSimplifyPassesThroughNonObjectSchema proves an input schema that is not a JSON object (or
// is empty) is returned untouched rather than mangled, so the transform never produces something
// the model could not otherwise have received.
func TestSimplifyPassesThroughNonObjectSchema(t *testing.T) {
	for _, raw := range []string{"", "not json", "[1,2,3]", "true"} {
		out := simplifyTool(llm.Tool{Name: "x", InputSchema: json.RawMessage(raw)})
		if string(out.InputSchema) != raw {
			t.Fatalf("non-object schema %q was rewritten to %q", raw, out.InputSchema)
		}
	}
}

// TestSimplifiedSchemasReachTheModel proves the option is wired end to end: with it set, the
// definitions the executor sends carry the stripped schema, not the original.
func TestSimplifiedSchemasReachTheModel(t *testing.T) {
	def := llm.Tool{
		Name:        "echo",
		Description: "echo back the input",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string","description":"the message"}}}`),
	}
	tool := Func(def, echoTool().Invoke)
	model := llmtest.NewScripted(llmtest.SayText("done"))
	exec := NewExecutor(model, WithTools(tool), WithSimplifiedSchemas())

	driveToDone(t, exec, 3)

	reqs := model.Requests()
	if len(reqs) == 0 || len(reqs[0].Tools) == 0 {
		t.Fatal("no tool definitions reached the model")
	}
	if strings.Contains(string(reqs[0].Tools[0].InputSchema), "description") {
		t.Fatalf("the model received an un-simplified schema: %s", reqs[0].Tools[0].InputSchema)
	}
}

// FuzzSimplifyTool proves the transform never panics and never corrupts a schema into invalid
// JSON: any input either round-trips to valid JSON or is passed through verbatim.
func FuzzSimplifyTool(f *testing.F) {
	f.Add(`{"type":"object","description":"x"}`)
	f.Add(`{"properties":{"a":{"description":"d","examples":[1,2]}}}`)
	f.Add(`[1,{"title":"t"}]`)
	f.Add(``)
	f.Add(`garbage`)
	f.Fuzz(func(t *testing.T, raw string) {
		out := simplifyTool(llm.Tool{Name: "x", InputSchema: json.RawMessage(raw)})
		// Either the schema was passed through unchanged, or it must be valid JSON.
		if string(out.InputSchema) == raw {
			return
		}
		if !json.Valid(out.InputSchema) {
			t.Fatalf("simplify produced invalid JSON from %q: %q", raw, out.InputSchema)
		}
	})
}
