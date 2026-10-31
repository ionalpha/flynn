package launch

import (
	_ "embed"
	"sort"
	"strings"
)

// A trusted chat-template name reaches llama.cpp in one of three ways, because the
// runtime reads --chat-template differently depending on the value. Jinja is on by
// default, and in that mode the value is the template's source text; only "chatml" is
// recognised as a name. Passing "llama3" or "gpt-oss" there makes the server render the
// literal word as the whole prompt, so a name has to be turned into something the
// runtime will actually apply:
//
//   - "chatml" is the one name the Jinja path expands itself.
//   - A name llama.cpp also ships as a legacy built-in is passed with --no-jinja, which
//     is the mode that looks names up. Those templates cannot carry native tool calls.
//   - A name with embedded source is passed as that source. These are the families whose
//     tool calling only exists in Jinja, so Flynn carries the reviewed template itself
//     instead of reading the one inside the weights.
//
// Whichever way it travels, the template is chosen by the catalog entry and never read
// from the model.

// gptOSSTemplate is OpenAI's published chat template for gpt-oss (the chat_template.jinja
// in openai/gpt-oss-20b at revision 6cee5e81ee83917806bbde320786a8fb61efebee,
// Apache-2.0, attributed in NOTICE), byte for byte the template the ggml-org GGUF
// conversion embeds. It renders the harmony format, including tool definitions and the
// reasoning_effort setting.
//
//go:embed templates/gpt-oss.jinja
var gptOSSTemplate string

// gemma4Template is Google's canonical Gemma 4 chat template (the chat_template.jinja in
// google/gemma-4-12B-it at revision 43c9d014b231474fa7212dbb48a762566c2b255d, Apache-2.0,
// attributed in NOTICE), as embedded in google/gemma-4-12B-it-qat-q4_0-gguf. llama.cpp has no built-in name for Gemma 4; it
// recognises the family from the template source, so the source has to be supplied.
//
//go:embed templates/gemma4.jinja
var gemma4Template string

// embeddedTemplates maps a trusted name to the Jinja source Flynn passes for it.
var embeddedTemplates = map[string]string{
	"gpt-oss": gptOSSTemplate,
	"gemma4":  gemma4Template,
}

// templateArgs returns the llama.cpp flags that make the runtime apply the trusted
// template name. BuildPlan has already checked that name is recognised.
func templateArgs(name string) []string {
	if src, ok := embeddedTemplates[name]; ok {
		return []string{"--chat-template", src}
	}
	if name == "chatml" {
		return []string{"--chat-template", name}
	}
	return []string{"--no-jinja", "--chat-template", name}
}

// TrustedChatTemplates lists the trusted template names in order, for a message that
// tells a person which names a flag accepts.
func TrustedChatTemplates() []string {
	names := make([]string, 0, len(knownChatTemplates))
	for name := range knownChatTemplates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NativeToolCalls reports whether the server can offer native tool calls under the
// trusted template name. The legacy built-ins run with --no-jinja, and a llama.cpp server
// in that mode rejects a request that carries tools, so a model on one of them can chat
// but cannot be an agent. A name that runs as Jinja can.
func NativeToolCalls(name string) bool {
	return KnownChatTemplate(name) && len(templateArgs(name)) == 2
}

// MatchTemplate names the trusted template that fits a chat template found in a model
// repository, and reports whether one does. The text is untrusted and is only searched
// for the control tokens each family is known by: it is never rendered, executed or
// passed on, and the answer is always a member of the trusted set, so a hostile template
// can choose which reviewed template is used but cannot supply its own.
//
// A caller treats the answer as a suggestion for a human to confirm. A template that
// mimics one family's tokens selects that family's reviewed template and nothing else.
func MatchTemplate(src string) (string, bool) {
	has := func(tokens ...string) bool {
		for _, t := range tokens {
			if !strings.Contains(src, t) {
				return false
			}
		}
		return true
	}
	switch {
	case has("<|start|>", "<|channel|>", "<|message|>"):
		return "gpt-oss", true
	case has("<|turn>", "<turn|>"):
		return "gemma4", true
	case has("<start_of_turn>"):
		return "gemma", true
	case has("<|start_header_id|>"):
		return "llama3", true
	case has("<|im_start|>"):
		return "chatml", true
	case has("[SYSTEM_PROMPT]"):
		return "mistral-v7", true
	case has("[INST]"):
		return "llama2", true
	case has("<|assistant|>", "<|end|>"):
		return "phi3", true
	}
	return "", false
}

// TemplateReasoning reports whether a chat template renders a reasoning phase: a think
// block, a reasoning-effort setting, or a thinking switch. Like [MatchTemplate] it only
// searches the untrusted text.
func TemplateReasoning(src string) bool {
	for _, marker := range []string{"<think>", "<|think|>", "enable_thinking", "reasoning_effort", "<|channel>thought"} {
		if strings.Contains(src, marker) {
			return true
		}
	}
	return false
}
