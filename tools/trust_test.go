package tools

import (
	"testing"

	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
)

// TestToolTrustLevels pins the trust each default tool's work carries, the input to the
// containment gate at the dispatch waist. The shell tool runs model-authored commands and
// must declare semi-trust so the waist requires kernel confinement for it; the structured
// file tools are the agent's own vetted code and must stay trusted (they do not declare a
// level), so they run wherever the agent does.
func TestToolTrustLevels(t *testing.T) {
	set := New(nil)
	byName := map[string]mission.Tool{}
	for _, tool := range set.Tools() {
		byName[tool.Def().Name] = tool
	}

	// The shell tool is semi-trusted: model-authored content.
	bash, ok := byName["bash"]
	if !ok {
		t.Fatal("the default toolset must include bash")
	}
	tw, ok := bash.(mission.TrustedWork)
	if !ok {
		t.Fatal("bash must declare a trust level (model-authored commands are not trusted)")
	}
	if tw.WorkTrust() != sandbox.TrustSemi {
		t.Fatalf("bash work trust = %v, want semi-trusted", tw.WorkTrust())
	}

	// Every structured file tool is the agent's own code: trusted, declaring no lower
	// level, so it is never refused for want of stronger isolation.
	for _, name := range []string{"read", "write", "edit", "glob", "grep"} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("the default toolset must include %q", name)
		}
		if _, declares := tool.(mission.TrustedWork); declares {
			t.Fatalf("%q must not declare a lower trust; the agent's own tools are trusted", name)
		}
	}
}
