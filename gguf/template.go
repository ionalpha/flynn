package gguf

// ChatTemplateDecision is the outcome of choosing the chat template to run a model
// with: the template to actually use, and whether the model tried to supply its own.
type ChatTemplateDecision struct {
	// Template is the template to run with. It is always the caller's trusted
	// template, never the model's, so the prompt contract cannot be set by the model.
	Template string
	// ModelSupplied reports that the model embedded its own chat template. That
	// template is ignored for the contract; the field lets a caller log or refuse a
	// model that ships an unexpected template.
	ModelSupplied bool
}

// ChooseChatTemplate decides the chat template to run a model with. It never returns
// the model's embedded template for use: a hostile GGUF can embed a template that
// rewrites the prompt contract to inject instructions at inference, so the contract is
// always the caller's trusted template. The decision reports whether the model tried
// to supply one, so a caller can record or refuse that.
func ChooseChatTemplate(meta *Metadata, trusted string) ChatTemplateDecision {
	_, modelSupplied := meta.ChatTemplate()
	return ChatTemplateDecision{
		Template:      trusted,
		ModelSupplied: modelSupplied,
	}
}
