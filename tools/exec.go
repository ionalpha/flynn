package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/ionalpha/flynn/llm"
)

type bashTool struct{ s *Set }

func (bashTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "bash",
		Description: "Run a shell command in the working directory and return its combined stdout and stderr. A non-zero exit is reported with the output, not hidden.",
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

func (t bashTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if in.Command == "" {
		return "", fmt.Errorf("bash: empty command")
	}

	name, args := shell(in.Command)
	//nolint:gosec // running a model-supplied command is this tool's entire purpose; isolation is the run sandbox, not the arg list
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = t.s.root
	out, err := cmd.CombinedOutput()
	result := string(out)

	if err != nil {
		// A command that ran but exited non-zero is not a tool failure: return its
		// output and exit code so the model can read stderr and react, the same way
		// a shell would. Only a failure to start (or a cancelled context) is an error.
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if result != "" {
				result += "\n"
			}
			return result + fmt.Sprintf("[exit status %d]", exit.ExitCode()), nil
		}
		return result, fmt.Errorf("bash: %w", err)
	}
	return result, nil
}

// shell returns the per-OS command that runs a shell command string: a POSIX
// shell everywhere except Windows, where cmd.exe is the only one guaranteed to be
// present.
func shell(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", command}
	}
	return "sh", []string{"-c", command}
}
