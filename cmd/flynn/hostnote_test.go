package main

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/mission"
)

// TestDefaultSystemPromptNamesTheHostShell: the agent writes the commands a check later
// runs, so its standing prompt has to say which shell reads them. Without it a model on
// Windows writes POSIX syntax that cmd.exe cannot run.
func TestDefaultSystemPromptNamesTheHostShell(t *testing.T) {
	if !strings.Contains(defaultSystemPrompt, mission.HostNote()) {
		t.Fatalf("the default system prompt does not carry the host note:\n%s", defaultSystemPrompt)
	}
}
