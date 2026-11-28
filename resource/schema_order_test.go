package resource_test

import (
	"testing"

	"github.com/ionalpha/flynn/resource"
)

// With several invalid properties, validation must name the same one each time:
// the first by name.
func TestSchemaErrorNamesTheSamePropertyEveryRun(t *testing.T) {
	s, err := resource.NewSchemaCompiler().Compile([]byte(`{"type":"object","additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	instance := map[string]any{"f": 1, "e": 1, "d": 1, "c": 1, "b": 1, "a": 1}
	first := s.Validate(instance)
	if first == nil {
		t.Fatal("no error")
	}
	for range 50 {
		if got := s.Validate(instance); got.Error() != first.Error() {
			t.Fatalf("error changed between runs: %q then %q", first, got)
		}
	}
}
