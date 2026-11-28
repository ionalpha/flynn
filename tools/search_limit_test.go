package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/sandbox"
)

// A search that reaches its own time limit returns what it has and says that it
// is partial.
func TestSearchTimeLimit(t *testing.T) {
	sb, err := sandbox.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(context.Background(), "a.txt", []byte("needle\n")); err != nil {
		t.Fatal(err)
	}
	s := New(sb)
	s.searchTime = time.Nanosecond
	for name, call := range map[string]func() (string, error){
		"grep": func() (string, error) {
			return grepTool{s}.Invoke(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
		},
		"glob": func() (string, error) {
			return globTool{s}.Invoke(context.Background(), json.RawMessage(`{"pattern":"**/*.txt"}`))
		},
	} {
		out, err := call()
		if err != nil || !strings.Contains(out, "search stopped after 1ns; results are partial") {
			t.Errorf("%s = %q, %v; want a partial-result note", name, out, err)
		}
	}
}
