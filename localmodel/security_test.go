package localmodel_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/localmodel"
)

// getAuthed reads a path of the fake runtime with the launch's token and decodes the JSON
// list it returns.
func getAuthed(t *testing.T, ep localmodel.Endpoint, path string) []string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ep.BaseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ep.Token.Expose())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out []string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The served runtime is untrusted code. It gets no credential of the host's: not the
// model-hub token the downloader uses, not provider keys, and not its own launch token
// through the environment or the command line.
func TestServerEnvHasNoSecrets(t *testing.T) {
	planted := map[string]string{
		"HF_TOKEN":                 "hub-secret-1f3a",
		"HUGGING_FACE_HUB_TOKEN":   "hub-secret-9c2d",
		"GITHUB_TOKEN":             "gh-secret-77aa",
		"OPENAI_API_KEY":           "oa-secret-55bb",
		"ANTHROPIC_API_KEY":        "an-secret-33cc",
		"AWS_SECRET_ACCESS_KEY":    "aws-secret-11dd",
		"FLYNN_SOME_SERVICE_TOKEN": "fl-secret-22ee",
	}
	for k, v := range planted {
		t.Setenv(k, v)
	}
	f := newFixture(t, fixtureOpts{})
	_, ep := serveOnce(t, f)

	env := strings.Join(getAuthed(t, ep, "/_env"), "\n")
	if len(env) == 0 {
		t.Fatal("the runtime reported an empty environment; the check would pass vacuously")
	}
	for k, v := range planted {
		if strings.Contains(env, v) {
			t.Errorf("the runtime inherited %s", k)
		}
	}
	if tok := ep.Token.Expose(); strings.Contains(env, tok) || strings.Contains(strings.Join(argvOf(t, ep), "\n"), tok) {
		t.Fatal("the launch token is in the runtime's environment or command line")
	}
}

// No option, field or method of the public API can skip or weaken the advisory gate or
// the version floor. The gate has no switch, so this fails the day someone adds one.
func TestNoOptionSkipsAdvisoryGate(t *testing.T) {
	bad := []string{"skip", "disable", "bypass", "ignore", "insecure", "unsafe", "force", "override", "allow", "lower", "reduce", "unpin", "nogate", "nofloor", "mincheck"}
	named := func(what, name string) {
		l := strings.ToLower(name)
		for _, b := range bad {
			if strings.Contains(l, b) {
				t.Errorf("%s %s reads as a way around the gate", what, name)
			}
		}
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(localmodel.Config{}), reflect.TypeOf(localmodel.ServeOptions{})} {
		for i := range typ.NumField() {
			named(typ.Name()+" field", typ.Field(i).Name)
			if k := typ.Field(i).Type.Kind(); k == reflect.Bool && typ.Name() == "Config" {
				t.Errorf("%s.%s is a boolean switch; a switch on provisioning could turn a check off", typ.Name(), typ.Field(i).Name)
			}
		}
	}
	host := reflect.TypeOf(&localmodel.Host{})
	for i := range host.NumMethod() {
		named("Host method", host.Method(i).Name)
	}
	// ServeOptions may carry only resource knobs; pin the full set so a new field is read.
	var got []string
	so := reflect.TypeOf(localmodel.ServeOptions{})
	for i := range so.NumField() {
		got = append(got, so.Field(i).Name)
	}
	if want := []string{"CtxSize", "CPUOnly"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ServeOptions fields = %v, want %v; review the new field against the four refusals", got, want)
	}
}

// At the default level the runtime is started without verbose or debug logging, which is
// what makes llama.cpp print prompt text, and without the web UI or other endpoints that
// expose prior prompts.
func TestServerLogsExcludePromptsByDefault(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	_, ep := serveOnce(t, f)
	for _, a := range argvOf(t, ep) {
		switch strings.ToLower(a) {
		case "-v", "--verbose", "--verbosity", "--log-verbose", "--log-verbosity", "-lv", "--slots", "--props", "--log-disable":
			t.Errorf("the runtime was started with %s", a)
		}
	}
	if _, ok := flagValue(argvOf(t, ep), "--api-key-file"); !ok {
		t.Fatal("the runtime was started without a key file")
	}
	found := false
	for _, a := range argvOf(t, ep) {
		if a == "--no-webui" {
			found = true
		}
	}
	if !found {
		t.Error("the web UI was not turned off")
	}
}
