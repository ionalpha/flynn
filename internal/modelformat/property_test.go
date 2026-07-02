package modelformat

import (
	"bytes"
	"testing"

	"pgregory.net/rapid"
)

// TestProp_CheckAgreesWithDetect pins the gate's core invariant over arbitrary input:
// Check admits a file exactly when Detect calls its format safe to parse, and reports
// the same format Detect does. So there is no input for which the gate and the detector
// disagree, and in particular none that Detect calls unsafe yet Check lets through.
func TestProp_CheckAgreesWithDetect(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		data := rapid.SliceOfN(rapid.Byte(), 0, 80).Draw(rt, "data")

		head := data
		if len(head) > headerLen {
			head = head[:headerLen]
		}
		detected := Detect(head)

		f, err := Check(bytes.NewReader(data))
		if f != detected {
			rt.Fatalf("Check reported %v but Detect reported %v for %q", f, detected, data)
		}
		if (err == nil) != detected.SafeToParse() {
			rt.Fatalf("Check err=%v disagrees with Detect(%v).SafeToParse()=%v for %q",
				err, detected, detected.SafeToParse(), data)
		}
	})
}
