package main

import "testing"

// TestRequireApprovalFlagAccumulates: the flag is repeatable, so naming two actions gates
// two, and the second occurrence does not overwrite the first the way a plain string flag
// would. A blank occurrence is dropped rather than becoming an action named the empty
// string, which would list a requirement nothing could ever satisfy.
func TestRequireApprovalFlagAccumulates(t *testing.T) {
	var list stringList
	for _, v := range []string{"shell", "  ", "net.fetch", "shell"} {
		if err := list.Set(v); err != nil {
			t.Fatalf("set %q: %v", v, err)
		}
	}
	if got := list.String(); got != "shell,net.fetch,shell" {
		t.Fatalf("accumulated %q, want every non-blank occurrence in order", got)
	}
	// A nil list reads as the empty string rather than panicking, since the flag package
	// calls String on the zero value while building its usage output.
	var empty *stringList
	if empty.String() != "" {
		t.Fatal("the zero flag value did not render as empty")
	}
}
