// Package tools is the agent's default toolset: the standard capable surface a
// coding agent needs to do real work - run a command, and read, write, edit,
// glob, and grep files - each exposed as a mission.Tool the model can call.
//
// The tools hold no host access of their own. Every command and file operation
// goes through a sandbox.Sandbox, which confines it to a working directory and
// (in stronger tiers) a container or microVM. So the tools are pure logic - the
// edit single-match rule, the grep binary skip, the bash exit reporting - and the
// isolation lives in one place beneath them, the same for every tier.
package tools

import (
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
)

// Set is the default toolset over a sandbox. Construct it with New and hand its
// tools to a mission executor.
type Set struct {
	sb sandbox.Sandbox
}

// New builds the default toolset executing through sb.
func New(sb sandbox.Sandbox) *Set {
	return &Set{sb: sb}
}

// Tools returns the full default toolset as mission.Tools, ready to register with
// an executor.
func (s *Set) Tools() []mission.Tool {
	return []mission.Tool{
		bashTool{s},
		readTool{s},
		writeTool{s},
		editTool{s},
		globTool{s},
		grepTool{s},
	}
}
