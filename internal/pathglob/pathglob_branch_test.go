package pathglob

import "testing"

func TestMayContainBelowShortPattern(t *testing.T) {
	p, err := Compile("a")
	if err != nil {
		t.Fatal(err)
	}
	if p.MayContain("a/b") {
		t.Error("a one-segment pattern cannot match below a/b")
	}
}

func TestEscapedBracketBeforeNegatedClass(t *testing.T) {
	p, err := Compile(`\[[!a]`)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Match("[b") || p.Match("[a") {
		t.Error("an escaped bracket must stay literal and the class stay negated")
	}
}

func TestCompileRejectsLaterUnclosedBrace(t *testing.T) {
	if _, err := Compile("{a,b}{c"); err == nil {
		t.Error("Compile accepted an unclosed second group")
	}
}
