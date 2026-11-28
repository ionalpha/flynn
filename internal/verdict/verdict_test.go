package verdict

import (
	"errors"
	"strings"
	"testing"
)

// TestZeroValueIsUnknown checks a verdict nobody set reads as neither a pass nor a
// fail, and still carries an error to report.
func TestZeroValueIsUnknown(t *testing.T) {
	var v Verdict
	if v.Outcome() != Unknown {
		t.Fatalf("zero Verdict outcome = %v, want unknown", v.Outcome())
	}
	if !errors.Is(v.Err(), errNoCause) {
		t.Fatalf("zero Verdict Err = %v, want errNoCause", v.Err())
	}
}

// TestErrIsSetExactlyForUnknown checks the guarantee the type documents: Err is
// non-nil only for a check that could not run.
func TestErrIsSetExactlyForUnknown(t *testing.T) {
	cause := errors.New("network down")
	for _, tc := range []struct {
		v       Verdict
		outcome Outcome
		wantErr bool
		reason  string
	}{
		{Pass(), Passed, false, ""},
		{Fail("too big"), Failed, false, "too big"},
		{Unevaluated(cause), Unknown, true, "network down"},
		{Unevaluated(nil), Unknown, true, errNoCause.Error()},
	} {
		if got := tc.v.Outcome(); got != tc.outcome {
			t.Errorf("%v: outcome = %v, want %v", tc.v, got, tc.outcome)
		}
		if got := tc.v.Err() != nil; got != tc.wantErr {
			t.Errorf("%v: Err set = %v, want %v", tc.v, got, tc.wantErr)
		}
		if got := tc.v.Reason(); got != tc.reason {
			t.Errorf("%v: Reason = %q, want %q", tc.v, got, tc.reason)
		}
	}
	if !errors.Is(Unevaluated(cause).Err(), cause) {
		t.Error("Unevaluated must keep its cause for errors.Is")
	}
}

func TestStringNamesTheOutcome(t *testing.T) {
	for v, want := range map[Verdict]string{
		Pass():                          "passed",
		Fail("x"):                       "failed: x",
		Unevaluated(errors.New("down")): "unknown: down",
	} {
		if got := v.String(); got != want {
			t.Errorf("String = %q, want %q", got, want)
		}
	}
	if got := Outcome(9).String(); !strings.Contains(got, "9") {
		t.Errorf("an unnamed outcome renders as %q", got)
	}
}
