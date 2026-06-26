package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/sandbox"
)

type bashTool struct{ s *Set }

// WorkTrust marks a shell command as semi-trusted: the command text is authored by the
// model, not the agent, so the waist requires kernel-confined isolation before it runs
// and refuses it on a host that cannot provide that. The other tools are the agent's own
// vetted code and stay trusted by not declaring a level.
func (bashTool) WorkTrust() sandbox.Trust { return sandbox.TrustSemi }

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
		return "", errors.New("bash: empty command")
	}
	res, err := t.s.sb.Exec(ctx, sandbox.Command{Line: in.Command})
	if err != nil {
		return res.Output, err
	}
	// A non-zero exit is not a tool failure: surface the output and exit code so the
	// model can read stderr and react, the way a shell would.
	if res.ExitCode != 0 {
		out := res.Output
		if out != "" {
			out += "\n"
		}
		return out + fmt.Sprintf("[exit status %d]", res.ExitCode), nil
	}
	return res.Output, nil
}
