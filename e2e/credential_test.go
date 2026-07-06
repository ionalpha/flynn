package e2e

import (
	"runtime"
	"strings"
	"testing"
)

// credSecret is the distinctive provider key the credential tests plant, so any leak of
// it into output, logs, or a child process is unambiguous.
const credSecret = "sk-e2e-LEAK-CANARY-7f3a91"

// TestAuthSetRefusesPipedSecret asserts the CLI will not read a secret from a pipe: a
// key handed on stdin is refused with a terminal-required error. This stops a key from
// landing in shell history or a script's process table, the way `echo $KEY | flynn auth
// set` would.
func TestAuthSetRefusesPipedSecret(t *testing.T) {
	in := newInstance(t)
	res := in.runInput([]byte(credSecret+"\n"), "auth", "set", "openai")
	requireExit(t, res, 1, "auth set with piped secret")
	requireContains(t, strings.ToLower(res.combined()), "terminal", "refusal names the terminal requirement")
	if strings.Contains(res.combined(), credSecret) {
		t.Fatalf("the refused key was echoed back:\n%s", res.combined())
	}
}

// TestProviderKeyNotInRunOutput plants the provider key in the environment (the
// non-interactive way a key is supplied) and runs a verbose goal. The key must never
// appear in the run's stdout or stderr, not even under -v, which prints tool arguments,
// outputs, and per-turn detail.
func TestProviderKeyNotInRunOutput(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("finished"))
	in := newInstance(t).withModel(fake)
	in.setEnv("OPENAI_API_KEY", credSecret)

	res := in.run("-no-learn", "-v", "goal", "do a small task")
	requireExit(t, res, 0, "verbose goal")
	if strings.Contains(res.combined(), credSecret) {
		t.Fatalf("provider key leaked into run output:\n%s", res.combined())
	}
}

// TestChildEnvScrubbed proves the scrubbed child environment: a command the agent runs
// cannot read the provider key. The model calls the shell tool to print the key through
// the host interpreter; the tool output must not contain the secret value, because the
// sandbox grants a child only a tiny credential-free baseline environment.
func TestChildEnvScrubbed(t *testing.T) {
	fake := newFakeOpenAIQueue(
		t,
		toolCall("c", "bash", `{"command":"`+echoKeyCommand()+`"}`),
		finalText("done"),
	)
	in := newInstance(t).withModel(fake)
	in.setEnv("OPENAI_API_KEY", credSecret)

	in.run("-no-learn", "goal", "print the environment")

	var sawToolOut bool
	for i := 1; i < fake.count(); i++ {
		for _, m := range fake.request(t, i).Messages {
			if m.Role != "tool" {
				continue
			}
			sawToolOut = true
			if strings.Contains(m.Content, credSecret) {
				t.Fatalf("provider key reached a child process env: %q", m.Content)
			}
		}
	}
	if !sawToolOut {
		t.Fatal("the shell command did not run; nothing to assert about child env")
	}
}

// echoKeyCommand prints the provider key through the host shell: cmd.exe reads
// %VAR%, a POSIX shell reads $VAR. An unset variable prints empty (or the literal token
// on cmd.exe), so a scrubbed env yields output without the secret value.
func echoKeyCommand() string {
	if runtime.GOOS == "windows" {
		return "echo KEY=[%OPENAI_API_KEY%]"
	}
	return "echo KEY=[$OPENAI_API_KEY]"
}
