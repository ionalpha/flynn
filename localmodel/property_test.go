package localmodel_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Property: no sequence of RaiseFloor calls naming a build at or below the floor in force
// changes the floor, and a call naming no version is refused without effect. The runtime
// floor can only rise, whatever a host passes in.
func TestProp_RaiseFloorNeverLowers(t *testing.T) {
	h := newFixture(t, fixtureOpts{}).open()
	before := h.Floor()
	digits := strings.TrimLeft(before, "bB")
	current, err := strconv.Atoi(digits)
	if err != nil {
		t.Fatalf("floor %q is not a build number: %v", before, err)
	}
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, current).Draw(rt, "build")
		if err := h.RaiseFloor(fmt.Sprintf("b%d", n), "PROP-LOWER"); err != nil {
			rt.Fatalf("a valid version was rejected: %v", err)
		}
		junk := rapid.StringMatching(`[a-z .-]{0,12}`).Draw(rt, "junk")
		_ = h.RaiseFloor(junk, "PROP-JUNK")
		if got := h.Floor(); got != before {
			rt.Fatalf("floor moved from %s to %s", before, got)
		}
	})
}

// Property: a credential in a model reference's userinfo or query never appears in the
// Assessment or in the error Classify returns, since both are built to be displayed.
func TestProp_ClassifyNeverEchoesACredential(t *testing.T) {
	h := newFixture(t, fixtureOpts{}).open()
	rapid.Check(t, func(rt *rapid.T) {
		secret := "Zq9" + rapid.StringMatching(`[A-Za-z0-9]{8,24}`).Draw(rt, "secret")
		ref := rapid.SampledFrom([]string{
			"https://user:%s@example.com/model.gguf",
			"https://%s@example.com/model.gguf",
			"https://example.com/model.gguf?token=%s",
			"https://example.com/a/b/model.gguf?X-Amz-Signature=%s&x=1",
		}).Draw(rt, "form")
		a, err := h.Classify(fmt.Sprintf(ref, secret))
		shown := fmt.Sprintf("%+v", a)
		if err != nil {
			shown += err.Error()
		}
		if strings.Contains(shown, secret) {
			rt.Fatalf("credential echoed: %s", shown)
		}
	})
}
