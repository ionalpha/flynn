package gguf

// ChatTemplateDecision is the chat template to run a model with, and whether the
// model tried to supply its own.
type ChatTemplateDecision struct {
	// Template is always the caller's trusted template, so the model cannot set the
	// prompt contract.
	Template string
	// ModelSupplied reports that the model embedded a chat template, which is
	// ignored, so a caller can log or refuse a model shipping one unexpectedly.
	ModelSupplied bool
}

// ChooseChatTemplate decides the chat template to run a model with. It never returns
// the embedded template for use, because a hostile GGUF can embed one that injects
// instructions at inference; the caller's trusted template always wins.
func ChooseChatTemplate(meta *Metadata, trusted string) ChatTemplateDecision {
	_, modelSupplied := meta.ChatTemplate()
	return ChatTemplateDecision{
		Template:      trusted,
		ModelSupplied: modelSupplied,
	}
}
