package launch

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/catalog"
)

// FuzzBuildPlan asserts that no configuration, however malformed its strings, can make
// BuildPlan panic or produce a plan that escapes loopback. An accepted plan must always
// bind 127.0.0.1 and never the wildcard address; a rejected one is fine.
func FuzzBuildPlan(f *testing.F) {
	f.Add("bin", "weights", "chatml", 8080, 0, "")
	f.Add("", "", "", 0, 0, "")
	f.Add("b", "w", "evil", 70000, -1, "key")
	f.Fuzz(func(t *testing.T, bin, weights, tmpl string, port, ctx int, apiKey string) {
		model := catalog.ModelSpec{ID: "ollama:f:1b", Kind: catalog.KindLocal, ChatTemplate: tmpl}
		plan, err := BuildPlan(Config{BinPath: bin, WeightsPath: weights, Model: model, Port: port, CtxSize: ctx, APIKey: apiKey})
		if err != nil {
			return // a rejected config is acceptable
		}
		if plan.Host != "127.0.0.1" {
			t.Fatalf("accepted plan binds non-loopback host %q", plan.Host)
		}
		joined := strings.Join(plan.Argv, "\x00")
		if strings.Contains(joined, "0.0.0.0") {
			t.Fatalf("accepted plan binds the wildcard address: %v", plan.Argv)
		}
		// An accepted plan only ever uses a known, trusted template.
		if !KnownChatTemplate(tmpl) {
			t.Fatalf("accepted a plan with an untrusted template %q", tmpl)
		}
	})
}
