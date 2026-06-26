package launch

import (
	"os"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/gguf"
)

// InspectTemplate reads the weights' GGUF metadata with the hardened reader, never the
// runtime's own parser, and decides the chat template to run with. The decision is
// always the trusted name, never the model's; the returned flag reports whether the
// model embedded a template of its own, so a caller can set Config.ModelEmbedsTemplate
// and record that an override was needed. It reads only the header, not the tensors.
func InspectTemplate(weightsPath, trusted string) (gguf.ChatTemplateDecision, error) {
	f, err := os.Open(weightsPath) //nolint:gosec // weightsPath is a verified weights file under Flynn's data dir
	if err != nil {
		return gguf.ChatTemplateDecision{}, fault.Wrap(fault.Terminal, "launch_open_weights", err)
	}
	defer func() { _ = f.Close() }()
	meta, err := gguf.ReadMetadata(f)
	if err != nil {
		return gguf.ChatTemplateDecision{}, fault.Wrap(fault.Terminal, "launch_read_gguf", err)
	}
	return gguf.ChooseChatTemplate(meta, trusted), nil
}
