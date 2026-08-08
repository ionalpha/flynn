package bundled

import (
	"testing"

	"github.com/ionalpha/flynn/skill/skillstyle"
)

// The pack ships in a public repository and is the artefact a reader judges the rest
// of the work by, so its prose is gated here rather than by an instruction to reread
// everything before opening a pull request. What the gate refuses has no legitimate
// use in a skill: three punctuation marks, the shape of an internal identifier, and a
// short list of names that mean nothing outside the workspace that produced them.
//
// It reads every file in every skill directory, not only SKILL.md. A reference
// document is read by the same person.
func TestPackProseIsAuthored(t *testing.T) {
	found, err := skillstyle.CheckFS(FS(), Root)
	if err != nil {
		t.Fatalf("check the pack: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("the pack carries marks a skill must not:\n%s", skillstyle.Report(found))
	}
}
