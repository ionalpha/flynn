package testkit

import "testing"

// recordTB is a TB that records a failure instead of aborting, so the helpers'
// failure paths can be asserted.
type recordTB struct{ failed bool }

func (r *recordTB) Helper()               {}
func (r *recordTB) Fatalf(string, ...any) { r.failed = true }

type goldenSample struct {
	A int
	B []string
}

func TestGoldenCreateMatchAndDetect(t *testing.T) {
	t.Chdir(t.TempDir()) // hermetic: testdata/ is created under a temp dir
	val := goldenSample{A: 7, B: []string{"x", "y"}}

	*updateGolden = true
	Golden(t, "sample", val) // writes testdata/sample.golden
	*updateGolden = false

	Golden(t, "sample", val) // identical value matches → no failure

	fake := &recordTB{}
	Golden(fake, "sample", goldenSample{A: 8}) // changed value must fail
	if !fake.failed {
		t.Fatal("Golden must fail when the value differs from the golden file")
	}

	missing := &recordTB{}
	Golden(missing, "does-not-exist", val) // absent golden file must fail
	if !missing.failed {
		t.Fatal("Golden must fail when the golden file is missing")
	}
}

func TestDiffDetectsMismatch(t *testing.T) {
	fake := &recordTB{}
	Diff(fake, 1, 2)
	if !fake.failed {
		t.Fatal("Diff must fail on unequal values")
	}
	Diff(t, []int(nil), []int{}) // EquateEmpty: nil and empty are equal
}
