package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/sandbox"
)

type bashTool struct{ s *Set }

// WorkTrust is semi-trusted because the model writes the command, so dispatch
// requires kernel confinement and refuses the call on a host without it. The
// other tools declare no level and stay trusted.
func (bashTool) WorkTrust() sandbox.Trust { return sandbox.TrustSemi }

func (bashTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "bash",
		Description: shellToolDescription(),
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["command"],
  "properties": {
    "command": {"type": "string", "description": "The shell command to run."}
  },
  "additionalProperties": false
}`),
	}
}

// shellToolDescription names the host's shell so the model writes for it:
// cmd.exe on Windows has no heredocs or POSIX utilities.
func shellToolDescription() string {
	if runtime.GOOS == "windows" {
		return "Run a command in the working directory through the Windows command interpreter (cmd.exe) and return its combined stdout and stderr. Use cmd.exe syntax, not POSIX shell syntax: there is no bash, so heredocs, pipelines into while, and POSIX-only tools are unavailable. For listing, finding, reading, writing, or editing files, prefer the dedicated glob, grep, read, write, and edit tools rather than shell commands. A non-zero exit is reported with the output, not hidden."
	}
	return "Run a shell command in the working directory through the POSIX shell (/bin/sh) and return its combined stdout and stderr. A non-zero exit is reported with the output, not hidden."
}

func (t bashTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if in.Command == "" {
		return "", errors.New("bash: empty command")
	}
	res, err := t.s.sb.Exec(ctx, sandbox.Command{Line: in.Command})
	if err != nil {
		return res.Output, err
	}
	// A non-zero exit is a result, so the model can read stderr and react.
	if res.ExitCode != 0 {
		out := res.Output
		if out != "" {
			out += "\n"
		}
		return out + fmt.Sprintf("[exit status %d]", res.ExitCode), nil
	}
	return res.Output, nil
}
