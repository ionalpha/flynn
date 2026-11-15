package launch

import (
	"io"
	"os"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/gguf"
)

// InspectTemplate reads the weights' GGUF header with the hardened reader, never the
// runtime's parser, and decides the chat template. The decision is always the trusted
// name; the flag reports whether the model embedded its own, so a caller can set
// Config.ModelEmbedsTemplate.
func InspectTemplate(weightsPath, trusted string) (gguf.ChatTemplateDecision, error) {
	f, err := os.Open(weightsPath) //nolint:gosec // weightsPath is a verified weights file under Flynn's data dir
	if err != nil {
		return gguf.ChatTemplateDecision{}, fault.Wrap(fault.Terminal, "launch_open_weights", err)
	}
	defer func() { _ = f.Close() }()
	return InspectTemplateFrom(f, trusted)
}

// InspectTemplateFrom is [InspectTemplate] over a reader the caller holds. A caller that
// has just hashed a file passes that descriptor, so the bytes parsed are the bytes whose
// digest was checked and no reopen by path leaves room for a swap.
func InspectTemplateFrom(r io.Reader, trusted string) (gguf.ChatTemplateDecision, error) {
	meta, err := gguf.ReadMetadata(r)
	if err != nil {
		return gguf.ChatTemplateDecision{}, fault.Wrap(fault.Terminal, "launch_read_gguf", err)
	}
	return gguf.ChooseChatTemplate(meta, trusted), nil
}
