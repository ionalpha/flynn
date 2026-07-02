package reliability

import (
	"encoding/json"
	"strings"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
)

// probeSeed pins decoding for every probe so a measurement is reproducible on a runtime that
// honors the seed. Greedy decoding (temperature zero) removes sampling noise from the score.
var probeSeed = &llm.Sampling{Seed: 1, Temperature: 0}

// probe is one reproducible challenge: a request to send and a grader that decides whether the
// response passed. The grader is the full definition of the capability under test, so a probe is
// self-contained and the scorer stays generic.
type probe struct {
	name  string
	dim   dimension
	req   llm.Request
	grade func(llm.Response) bool
}

// Tool schemas used by the battery. They are deliberately small and ordinary, the shape an agent
// meets constantly, since the point is to measure dependability on routine calls, not to trip the
// model with exotic schemas.
var (
	weatherTool = llm.Tool{
		Name:        "get_weather",
		Description: "Get the current weather for a location.",
		InputSchema: json.RawMessage(`{"type":"object","required":["location"],"properties":{"location":{"type":"string"}},"additionalProperties":false}`),
	}
	readTool = llm.Tool{
		Name:        "read_file",
		Description: "Read a file from the working directory.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}},"additionalProperties":false}`),
	}
	eventTool = llm.Tool{
		Name:        "create_event",
		Description: "Create a calendar event.",
		InputSchema: json.RawMessage(`{"type":"object","required":["title","allDay"],"properties":{"title":{"type":"string"},"allDay":{"type":"boolean"}},"additionalProperties":false}`),
	}
)

// ask builds a single-user-turn request offering the given tools, with decoding pinned.
func ask(prompt string, tools ...llm.Tool) llm.Request {
	return llm.Request{
		Messages: []llm.Message{llm.Text(llm.RoleUser, prompt)},
		Tools:    tools,
		Sampling: probeSeed,
	}
}

// battery is the fixed probe set. It is intentionally a pure function returning a fresh slice so
// the set is immutable and a caller cannot accumulate state between runs. The mix covers the
// three dimensions with several probes each, so a score is a fraction rather than a single
// pass/fail.
func battery() []probe {
	return []probe{
		// Tool-call: a clear need for a specific tool should produce a well-formed call to it.
		{
			"weather_call", dimToolCall, ask("What is the weather in Paris right now?", weatherTool),
			func(r llm.Response) bool { return callsTool(r, "get_weather") },
		},
		{
			"read_call", dimToolCall, ask("Read the file config.yaml and tell me what is in it.", readTool),
			func(r llm.Response) bool { return callsTool(r, "read_file") },
		},
		{
			"event_call", dimToolCall, ask("Add an all-day calendar event titled Launch.", eventTool),
			func(r llm.Response) bool { return callsTool(r, "create_event") },
		},

		// Structured output: when a call is emitted, its arguments must validate against the
		// schema, the failure that burns agent turns invisibly under quantization.
		{
			"weather_args", dimStructured, ask("What is the weather in Berlin?", weatherTool),
			gradeSchema("get_weather", weatherTool.InputSchema),
		},
		{
			"read_args", dimStructured, ask("Read the file notes.md.", readTool),
			gradeSchema("read_file", readTool.InputSchema),
		},
		{
			"event_args", dimStructured, ask("Create an all-day event called Standup.", eventTool),
			gradeSchema("create_event", eventTool.InputSchema),
		},

		// Instruction-following: direct, checkable constraints, including the discipline of NOT
		// calling a tool when told to answer directly.
		{
			"exact_word", dimInstruction, ask("Reply with exactly the single word READY and nothing else."),
			func(r llm.Response) bool {
				return strings.EqualFold(strings.TrimSpace(r.Message.TextContent()), "READY")
			},
		},
		{
			"no_tool", dimInstruction, ask("Do not use any tools. Just state the capital of France in your reply.", readTool),
			func(r llm.Response) bool {
				return r.StopReason != llm.StopToolUse && len(r.Message.ToolUses()) == 0 &&
					strings.Contains(strings.ToLower(r.Message.TextContent()), "paris")
			},
		},
		{
			"one_word", dimInstruction, ask("In one word, what color is a clear daytime sky?"),
			func(r llm.Response) bool {
				return len(strings.Fields(strings.TrimSpace(r.Message.TextContent()))) == 1
			},
		},
	}
}

// callsTool reports whether the response is a well-formed request to call the named tool: the
// turn ended for tool use, and it carries a tool call of that name whose input is parseable JSON.
// A malformed call (wrong tool, no call, or unparseable arguments) fails, which is precisely the
// reliability the probe measures.
func callsTool(r llm.Response, name string) bool {
	if r.StopReason != llm.StopToolUse {
		return false
	}
	for _, tu := range r.Message.ToolUses() {
		if tu.Name == name && json.Valid(tu.Input) {
			return true
		}
	}
	return false
}

// gradeSchema builds a grader that passes when the response calls the named tool with arguments
// that validate against schema. It compiles the schema once with the same engine the resource
// store uses, so adherence is judged consistently with the rest of the system. A response that
// emits no matching call fails, since absent arguments cannot be schema-valid.
func gradeSchema(name string, schema json.RawMessage) func(llm.Response) bool {
	validator, err := resource.NewSchemaCompiler().Compile(schema)
	return func(r llm.Response) bool {
		if err != nil || r.StopReason != llm.StopToolUse {
			return false
		}
		for _, tu := range r.Message.ToolUses() {
			if tu.Name != name {
				continue
			}
			var args any
			if json.Unmarshal(tu.Input, &args) != nil {
				return false
			}
			return validator.Validate(args) == nil
		}
		return false
	}
}
