package localmodel_test

import (
	"errors"
	"testing"

	"github.com/ionalpha/flynn/localmodel"
	"github.com/ionalpha/flynn/sandbox"
)

// An entry the user added, at the local tier, is served: the host holds it in its catalog and
// fetches it by its digest like any other. It is classified by the origin it names, so it
// needs a microvm and is refused where the host gives less, and it is not served as the
// trusted, pinned source a blessed entry is.
func TestOverlayEntryIsServedUnderTheIsolationItsOriginNeeds(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.model = entryAt(f, "mine:1b", localmodel.TrustLocal, localmodel.Source{Publisher: "acme", URL: "https://example.test/mine.gguf"})
	h := f.open()

	a, err := h.Classify(f.model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Catalog || a.Trust != "untrusted" || a.Required != sandbox.ContainmentMicroVM.String() {
		t.Fatalf("overlay entry = %+v; want untrusted, needing a microvm, and not a catalog source", a)
	}

	for _, level := range []sandbox.Containment{sandbox.ContainmentNone, sandbox.ContainmentKernel} {
		h.SetProvided(level)
		_, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
		if !errors.Is(err, localmodel.ErrNotAdmitted) || errors.Is(err, localmodel.ErrNotServable) {
			t.Fatalf("Serve under %s = %v; want ErrNotAdmitted", level, err)
		}
	}
	if f.hits.Load() != 0 {
		t.Fatal("an entry the host refused caused downloads")
	}

	h.SetProvided(sandbox.ContainmentMicroVM)
	ep, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
	if err != nil {
		t.Fatalf("Serve under a microvm = %v; want the entry served", err)
	}
	if ep.ModelID != f.model.ID || ep.Port == 0 || ep.Token.Empty() {
		t.Fatalf("incomplete endpoint: %+v", ep)
	}
	if f.hits.Load() == 0 {
		t.Fatal("the entry was served without fetching its runtime and weights")
	}
}
