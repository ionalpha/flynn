package e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
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

	out := toolOutputs(t, fake)
	if strings.Contains(out, credSecret) {
		t.Fatalf("provider key reached a child process env: %q", out)
	}
	// The echo's own text is the proof the command ran. A refusal is tool output too, and a
	// test that only looked for output would pass on a shell that never ran anything.
	requireContains(t, out, "KEY=[", "the shell command ran")
}

// toolOutputs joins every tool result the model was sent back over the run.
func toolOutputs(t *testing.T, fake *fakeOpenAI) string {
	t.Helper()
	var out []string
	for i := 1; i < fake.count(); i++ {
		for _, m := range fake.request(t, i).Messages {
			if m.Role == "tool" {
				out = append(out, m.Content)
			}
		}
	}
	return strings.Join(out, "\n")
}

// TestDefaultShellRunsACommand runs a goal with no shell flags on the host OS and requires
// the command to have run: its output carries the marker it printed, and no refusal. Where
// the platform cannot hide the credential locations the operator is told so on stderr, and
// nowhere else.
func TestDefaultShellRunsACommand(t *testing.T) {
	const marker = "shell-ran-e2e-5c1d"
	fake := newFakeOpenAIQueue(t, toolCall("c", "bash", `{"command":"echo `+marker+`"}`), finalText("done"))
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "run a command")
	requireExit(t, res, 0, "default goal")
	out := toolOutputs(t, fake)
	requireContains(t, out, marker, "the shell command ran under the default policy")
	if strings.Contains(out, "cannot hide") {
		t.Fatalf("the default shell refused the command: %q", out)
	}
	noticed := strings.Contains(res.stderr, "cannot hide paths")
	if noticed != (runtime.GOOS == "windows") {
		t.Fatalf("credential notice on stderr = %v on %s:\n%s", noticed, runtime.GOOS, res.stderr)
	}
}

// TestDefaultShellCannotReadSSHKeys plants a key under the instance's ~/.ssh and has the model
// print it. Linux and macOS hide the directory, so the canary never reaches the model. Windows
// cannot hide a path from a command: there the canary is readable, which is the documented
// limit, and the test holds the record to saying so, and holds --require-credential-hiding to
// refusing the command.
func TestDefaultShellCannotReadSSHKeys(t *testing.T) {
	const canary = "ssh-key-CANARY-e2e-91b0"
	in := newInstance(t)
	key := filepath.Join(in.home, ".ssh", "id_ed25519")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte(canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := "cat '" + key + "'; echo read-attempted"
	if runtime.GOOS == "windows" {
		read = `type "` + key + `" & echo read-attempted`
	}
	script := func() *fakeOpenAI {
		return newFakeOpenAIQueue(t, toolCall("c", "bash", `{"command":"`+jsonEscape(read)+`"}`), finalText("done"))
	}

	fake := script()
	res := in.withModel(fake).run("-no-learn", "goal", "print the ssh key")
	requireExit(t, res, 0, "goal reading the ssh key")
	out := toolOutputs(t, fake)
	requireContains(t, out, "read-attempted", "the read command ran")

	path, exp := in.export(in.runID(res))
	requireExit(t, exp, 0, "spine export")
	record, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	posture := recordedPosture(t, record)

	if runtime.GOOS != "windows" {
		if strings.Contains(out, canary) {
			t.Fatalf("the shell read ~/.ssh under the default policy: %q", out)
		}
		if posture["credentials"] != "hidden" {
			t.Fatalf("posture = %v, want the credentials recorded hidden", posture)
		}
		return
	}
	if !strings.Contains(out, canary) {
		t.Fatalf("Windows cannot hide ~/.ssh, so the probe should read it; it read %q", out)
	}
	if posture["credentials"] != "readable" || posture["credentials_reason"] != "platform" {
		t.Fatalf("posture = %v, want the credentials recorded readable because of the platform", posture)
	}

	strict := script()
	res = in.withModel(strict).run("-no-learn", "-require-credential-hiding", "goal", "print the ssh key")
	out = toolOutputs(t, strict)
	if strings.Contains(out, canary) || strings.Contains(out, "read-attempted") {
		t.Fatalf("--require-credential-hiding ran the command: %q", out)
	}
	requireContains(t, out, "cannot hide", "the strict refusal names why")
	if strings.Contains(res.stderr, "cannot hide paths") {
		t.Errorf("the strict run announced readable credentials it never granted:\n%s", res.stderr)
	}
}

// recordedPosture decodes an exported record and returns the payload of its one shell
// posture event.
func recordedPosture(t *testing.T, record []byte) map[string]any {
	t.Helper()
	// Each event is held as its own encoded bytes, the form its signature covers.
	var r struct {
		Events [][]byte `cbor:"events"`
	}
	if err := cbor.Unmarshal(record, &r); err != nil {
		t.Fatalf("decode the exported record: %v", err)
	}
	var found []map[string]any
	for _, raw := range r.Events {
		var e struct {
			Type    string         `cbor:"type"`
			Payload map[string]any `cbor:"payload"`
		}
		if err := cbor.Unmarshal(raw, &e); err != nil {
			t.Fatalf("decode a recorded event: %v", err)
		}
		if e.Type == "sandbox.posture" {
			found = append(found, e.Payload)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d sandbox.posture events in the record, want one", len(found))
	}
	return found[0]
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
