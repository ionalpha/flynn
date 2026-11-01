package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/ionalpha/flynn/internal/vault"
)

// The live-model lane runs real tasks against a real model through the binary under test,
// and is skipped unless FLYNN_E2E_REAL=1. Point FLYNN_E2E_BIN at a published release
// binary to test the bytes that ship; without it the suite builds its own, as everywhere.
//
// A live model's wording is not stable, so no assertion here reads it. A scenario asserts on
// the sealed run record (which tools were called, which actions were refused, whether the
// run converged) and on what the host can observe independently of the model: a file on
// disk, a connection a listener did or did not receive. A scenario that cannot reach its
// prerequisite (a key, a toolchain, a local model) skips and says which.
//
// Every goal goes through liveGoal or liveStart, which add the spend caps, so a scenario
// cannot run uncapped by forgetting them.

const (
	liveGate = "FLYNN_E2E_REAL"

	// defaultLiveMaxCost and defaultLiveMaxTokens cap each live goal. They are generous for
	// the small tasks here and small next to what a runaway loop would spend.
	defaultLiveMaxCost   = "0.50"
	defaultLiveMaxTokens = 400000

	// defaultLiveTimeout bounds one live invocation; a real model, and a local one most of
	// all, takes far longer per turn than the scripted server the default timeout fits.
	defaultLiveTimeout = 8 * time.Minute
)

// requireLive skips the test unless the lane is switched on.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv(liveGate) != "1" {
		t.Skip("set " + liveGate + "=1 to run the opt-in live-model lane")
	}
}

// hostedProvider is a hosted model provider the lane can reach, with the key variable the
// binary reads and a small, cheap model to run it against.
type hostedProvider struct {
	name   string
	model  string
	keyEnv string
}

// hostedProviders lists the providers in the order a default model is chosen. The model
// can be replaced with FLYNN_E2E_REAL_MODEL_<NAME> (for example
// FLYNN_E2E_REAL_MODEL_OPENAI=gpt-5.5), since a provider retires model names.
var hostedProviders = []hostedProvider{
	{"anthropic", "claude-haiku-4-5-20251001", "ANTHROPIC_API_KEY"},
	{"openai", "gpt-5-mini", "OPENAI_API_KEY"},
	{"gemini", "gemini-2.5-flash", "GEMINI_API_KEY"},
	{"deepseek", "deepseek-chat", "DEEPSEEK_API_KEY"},
}

// spec is the provider:model string the binary takes for this provider.
func (p hostedProvider) spec() string {
	model := p.model
	if v := os.Getenv("FLYNN_E2E_REAL_MODEL_" + strings.ToUpper(p.name)); v != "" {
		model = v
	}
	return p.name + ":" + model
}

// hasKey reports whether this provider's key can be found (see [providerKey]).
func (p hostedProvider) hasKey() bool { return providerKey(p.keyEnv) != "" }

// vaultKeys holds each key [providerKey] read from the operator's vault, so the keychain
// or sealed file is opened once per key and not once per scenario.
var vaultKeys sync.Map // keyEnv -> string ("" when the vault has none)

// providerKey returns the value of a hosted provider's key: the environment variable when
// it is set, otherwise the entry `flynn auth set` stored under the same name in the
// operator's vault, or "" when neither has it. The vault is the one at
// FLYNN_E2E_VAULT_DIR, or the release install's data directory by default.
//
// The vault is read only with the lane switched on, so the default run never touches a
// keychain. The key it finds is passed to the binary under test in its environment,
// because every instance has its own empty data directory and so an empty vault of its
// own.
func providerKey(keyEnv string) string {
	if v := os.Getenv(keyEnv); v != "" {
		return v
	}
	if os.Getenv(liveGate) != "1" {
		return ""
	}
	if v, ok := vaultKeys.Load(keyEnv); ok {
		return v.(string)
	}
	dir := os.Getenv("FLYNN_E2E_VAULT_DIR")
	if dir == "" {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(cfg, "flynn")
	}
	val := ""
	if s, err := vault.New(dir).Lookup(context.Background(), keyEnv); err == nil {
		val = s.Expose()
	}
	vaultKeys.Store(keyEnv, val)
	return val
}

// liveModel returns the model spec the scenarios share: FLYNN_E2E_REAL_MODEL when set,
// otherwise the first hosted provider whose key is present. It skips the test when there
// is nothing to run against.
func liveModel(t *testing.T) string {
	t.Helper()
	requireLive(t)
	if v := os.Getenv("FLYNN_E2E_REAL_MODEL"); v != "" {
		provider, _, _ := strings.Cut(v, ":")
		for _, p := range hostedProviders {
			if p.name == provider && !p.hasKey() {
				t.Skipf("FLYNN_E2E_REAL_MODEL names the %s provider but %s is neither set nor in the vault", provider, p.keyEnv)
			}
		}
		return v
	}
	for _, p := range hostedProviders {
		if p.hasKey() {
			return p.spec()
		}
	}
	t.Skip("no hosted provider key in the environment or the vault (set or `flynn auth set` one of ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY, DEEPSEEK_API_KEY) and FLYNN_E2E_REAL_MODEL is unset")
	return ""
}

// liveInstance is an isolated instance pointed at the real provider the spec names. It
// carries that provider's key from the ambient environment and no other, plus the base URL
// of a local runtime when one is configured.
func liveInstance(t *testing.T, spec string) *instance {
	t.Helper()
	in := newInstance(t)
	in.model = spec
	in.timeout = defaultLiveTimeout
	if v := os.Getenv("FLYNN_E2E_REAL_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("FLYNN_E2E_REAL_TIMEOUT=%q: %v", v, err)
		}
		in.timeout = d
	}
	provider, _, _ := strings.Cut(spec, ":")
	for _, p := range hostedProviders {
		if p.name == provider {
			if v := providerKey(p.keyEnv); v != "" {
				in.setEnv(p.keyEnv, v)
			}
		}
	}
	if base := localBaseEnv(provider); base != "" {
		if v := os.Getenv(base); v != "" {
			in.setEnv(base, v)
		}
	}
	return in
}

// localBaseEnv names the base-URL variable of a local runtime provider, or "" for any
// other provider.
func localBaseEnv(provider string) string {
	switch provider {
	case "ollama":
		return "OLLAMA_BASE_URL"
	case "llamacpp":
		return "LLAMACPP_BASE_URL"
	case "lmstudio":
		return "LMSTUDIO_BASE_URL"
	}
	return ""
}

// liveCaps returns the spend-cap flags every live goal carries. FLYNN_E2E_REAL_MAX_COST
// and FLYNN_E2E_REAL_MAX_TOKENS replace the defaults.
func liveCaps() []string {
	cost := defaultLiveMaxCost
	if v := os.Getenv("FLYNN_E2E_REAL_MAX_COST"); v != "" {
		cost = v
	}
	tokens := strconv.Itoa(defaultLiveMaxTokens)
	if v := os.Getenv("FLYNN_E2E_REAL_MAX_TOKENS"); v != "" {
		tokens = v
	}
	return []string{"-max-cost", cost, "-max-tokens", tokens}
}

// liveArgs builds a goal invocation: the spend caps, the caller's flags, then the goal.
func liveArgs(objective string, flags []string) []string {
	args := append(liveCaps(), "-no-learn")
	args = append(args, flags...)
	return append(args, "goal", objective)
}

// liveGoal runs a goal to completion under the spend caps, with learning off. flags are
// the run's own flags (for example "-allow-network"), placed before the subcommand.
func (in *instance) liveGoal(objective string, flags ...string) result {
	in.t.Helper()
	return in.run(liveArgs(objective, flags)...)
}

// liveGoalLearning is liveGoal with the learning loop on, for the scenarios that need a
// run to leave memory and skills behind.
func (in *instance) liveGoalLearning(objective string, flags ...string) result {
	in.t.Helper()
	return in.run(append(liveCaps(), append(flags, "goal", objective)...)...)
}

// liveStart is liveGoal in the background, for the scenarios that act on the run while it
// goes: kill, steer, a hard kill.
func (in *instance) liveStart(objective string, flags ...string) *running {
	in.t.Helper()
	return in.start(liveArgs(objective, flags)...)
}

// have skips the test unless the named tool is on the PATH.
func have(t *testing.T, tool string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s is not on the PATH", tool)
	}
}

// runRecord is a sealed run's exported record, decoded.
type runRecord struct {
	events []recEvent
}

// recEvent is one event of the record. Session events (tool calls and results, turns,
// convergence) carry their body as JSON inside the payload; session holds it decoded.
type recEvent struct {
	typ     string
	payload map[string]any
	session map[string]any
}

// loadRecord exports the run's record and decodes it, failing the test when the run is
// not sealed or the export does not decode.
func (in *instance) loadRecord(runID string) *runRecord {
	in.t.Helper()
	path, res := in.export(runID)
	requireExit(in.t, res, 0, "spine export")
	raw, err := os.ReadFile(path)
	if err != nil {
		in.t.Fatal(err)
	}
	// Each event is held as its own encoded bytes, the form its signature covers.
	var doc struct {
		Events [][]byte `cbor:"events"`
	}
	if err := cbor.Unmarshal(raw, &doc); err != nil {
		in.t.Fatalf("decode the exported record: %v", err)
	}
	rec := &runRecord{}
	for _, b := range doc.Events {
		var e struct {
			Type    string         `cbor:"type"`
			Payload map[string]any `cbor:"payload"`
		}
		if err := cbor.Unmarshal(b, &e); err != nil {
			in.t.Fatalf("decode a recorded event: %v", err)
		}
		ev := recEvent{typ: e.Type, payload: e.Payload}
		if body, ok := e.Payload["event"].(string); ok {
			_ = json.Unmarshal([]byte(body), &ev.session)
		}
		rec.events = append(rec.events, ev)
	}
	return rec
}

// recCall is one call the model made: the tool, the id its result is filed under, and its
// arguments.
type recCall struct {
	id    string
	tool  string
	input map[string]any
}

// command returns the shell command a bash call ran, or "" for any other call.
func (c recCall) command() string {
	s, _ := c.input["command"].(string)
	return s
}

// calls returns the tool calls the model made, in order. With names it returns only those
// tools.
func (r *runRecord) calls(names ...string) []recCall {
	var out []recCall
	for _, e := range r.events {
		if e.session["kind"] != "tool.call" {
			continue
		}
		tool, _ := e.session["tool"].(string)
		if len(names) > 0 && !containsString(names, tool) {
			continue
		}
		c := recCall{tool: tool}
		c.id, _ = e.session["toolUseID"].(string)
		c.input, _ = e.session["input"].(map[string]any)
		out = append(out, c)
	}
	return out
}

// result returns the output of the call with the given id and whether it was an error.
func (r *runRecord) result(id string) (string, bool) {
	for _, e := range r.events {
		if e.session["kind"] == "tool.result" && e.session["toolUseID"] == id {
			out, _ := e.session["result"].(string)
			isErr, _ := e.session["isError"].(bool)
			return out, isErr
		}
	}
	return "", false
}

// rejections returns the error codes of the actions the dispatch waist refused, in order.
func (r *runRecord) rejections() []string {
	var out []string
	for _, e := range r.events {
		if e.typ == "dispatch.rejected" {
			code, _ := e.payload["error_code"].(string)
			out = append(out, code)
		}
	}
	return out
}

// has reports whether the record holds an event of the given type.
func (r *runRecord) has(typ string) bool {
	for _, e := range r.events {
		if e.typ == typ {
			return true
		}
	}
	return false
}

// converged reports whether the run ended by converging.
func (r *runRecord) converged() bool { return r.has("session.converged") }

// posture returns the payload of the record's one sandbox.posture event.
func (r *runRecord) posture(t *testing.T) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, e := range r.events {
		if e.typ == "sandbox.posture" {
			found = append(found, e.payload)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d sandbox.posture events in the record, want one", len(found))
	}
	return found[0]
}

// outputs joins every tool result in the record, for a search for a planted canary.
func (r *runRecord) outputs() string {
	var out []string
	for _, e := range r.events {
		if e.session["kind"] == "tool.result" {
			s, _ := e.session["result"].(string)
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

// sealedRun asserts the run's record is intact and that no action ran without admission,
// then returns it decoded. A live scenario ends here so that what it asserts on is a record
// that passed those two checks. The ground-truth check is left to the scenario: whether a
// run's claimed success is backed by a passing check depends on what the model did.
func (in *instance) sealedRun(res result) *runRecord {
	in.t.Helper()
	id := in.runID(res)
	verified := in.verify(id)
	requireContains(in.t, verified.stdout, "integrity:    VERIFIED", "spine verify of the live run")
	requireContains(in.t, verified.stdout, "governance:   OK", "spine verify of the live run")
	return in.loadRecord(id)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
