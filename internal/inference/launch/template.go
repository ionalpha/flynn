package launch

import (
	_ "embed"
	"sort"
	"strings"
)

// A trusted template name reaches llama.cpp in one of three ways. With Jinja on, the
// default, --chat-template takes source text and recognises only "chatml" as a name;
// passing "llama3" there renders the literal word as the whole prompt. So:
//
//   - "chatml" is passed as the name.
//   - A name llama.cpp ships as a legacy built-in is passed with --no-jinja, the mode
//     that looks names up. Those templates cannot carry native tool calls.
//   - A name with embedded source is passed as that source, for families whose tool
//     calling exists only in Jinja.
//
// Either way the catalog entry chooses the template; it is never read from the model.

// gptOSSTemplate is OpenAI's chat template for gpt-oss (chat_template.jinja in
// openai/gpt-oss-20b at revision 6cee5e81ee83917806bbde320786a8fb61efebee,
// Apache-2.0, attributed in NOTICE), byte for byte what the ggml-org GGUF embeds.
//
//go:embed templates/gpt-oss.jinja
var gptOSSTemplate string

// gemma4Template is Google's Gemma 4 chat template (chat_template.jinja in
// google/gemma-4-12B-it at revision 43c9d014b231474fa7212dbb48a762566c2b255d, Apache-2.0,
// attributed in NOTICE), byte for byte. llama.cpp has no built-in name for Gemma 4 and
// recognises the family from the source, so the source is supplied.
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

// TrustedChatTemplates lists the trusted template names in order, for an error message.
func TrustedChatTemplates() []string {
	names := make([]string, 0, len(knownChatTemplates))
	for name := range knownChatTemplates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NativeToolCalls reports whether the server can offer native tool calls under the
// template name. A legacy built-in runs with --no-jinja, where llama.cpp rejects a request
// carrying tools, so a model on one can chat but cannot be an agent.
func NativeToolCalls(name string) bool {
	return KnownChatTemplate(name) && len(templateArgs(name)) == 2
}

// MatchTemplate names the trusted template that fits a chat template found in a model
// repository. The text is untrusted, so it is only searched for each family's control
// tokens, never rendered or passed on. The answer is always a trusted name, so a hostile
// template can pick which reviewed template runs but cannot supply its own. A caller
// treats the answer as a suggestion for a human to confirm.
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
// block, a reasoning-effort setting or a thinking switch. Like [MatchTemplate] it only
// searches the untrusted text.
func TemplateReasoning(src string) bool {
	for _, marker := range []string{"<think>", "<|think|>", "enable_thinking", "reasoning_effort", "<|channel>thought"} {
		if strings.Contains(src, marker) {
			return true
		}
	}
	return false
}
