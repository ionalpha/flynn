// Package tools is the agent's default toolset: run a command, and read,
// write, edit, glob and grep files.
//
// The tools have no host access. Every operation goes through a
// sandbox.Sandbox, so isolation is the same code for every tier.
package tools

import (
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
)

// Set is the default toolset over a sandbox.
type Set struct {
	sb sandbox.Sandbox
}

func New(sb sandbox.Sandbox) *Set {
	return &Set{sb: sb}
}

// Tools returns every tool in the set as [mission.Tool] values.
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
