package flow

import (
	"strings"
	"testing"
)

func TestDecodeValidFlow(t *testing.T) {
	raw := []byte(`{
      "steps": [
        {"id": "fetch", "op": "http", "http": {"url": "https://api/{{config.id}}"}},
        {"op": "return", "return": {"value": "{{steps.fetch.body}}"}}
      ]
    }`)
	f, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(f.Steps) != 2 {
		t.Fatalf("got %d steps", len(f.Steps))
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", `{"steps":[]}`, "at least one step"},
		{"op/action mismatch", `{"steps":[{"op":"http","transform":{"value":"1"}}]}`, "exactly one action"},
		{"two actions", `{"steps":[{"op":"http","http":{"url":"x"},"transform":{"value":"1"}}]}`, "exactly one action"},
		{"unknown op", `{"steps":[{"op":"frobnicate"}]}`, "exactly one action"},
		{"duplicate id", `{"steps":[{"id":"a","op":"return","return":{}},{"id":"a","op":"return","return":{}}]}`, "duplicate step id"},
		{"transform no mode", `{"steps":[{"op":"transform","transform":{"source":"config"}}]}`, "exactly one of"},
		{"transform two modes", `{"steps":[{"op":"transform","transform":{"value":"1","filter":"it"}}]}`, "exactly one of"},
		{"loop no source", `{"steps":[{"op":"loop","loop":{"body":[{"op":"return","return":{}}]}}]}`, "over or a positive count"},
		{"http no url", `{"steps":[{"op":"http","http":{}}]}`, "needs a url"},
		{"call no tool", `{"steps":[{"op":"call","call":{}}]}`, "needs a tool"},
		{"bad expr", `{"steps":[{"op":"transform","transform":{"value":"1 +"}}]}`, "flow:"},
		{"bad template", `{"steps":[{"op":"http","http":{"url":"a {{ b"}}]}`, "flow:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode([]byte(c.raw))
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

// TestValidateRecursesIntoBranches proves a malformed step nested in a condition or
// loop body is still caught.
func TestValidateRecursesIntoBranches(t *testing.T) {
	raw := `{"steps":[{"op":"condition","condition":{"if":"true","then":[{"op":"http","http":{}}]}}]}`
	if _, err := Decode([]byte(raw)); err == nil {
		t.Fatal("expected the nested http step with no url to be rejected")
	}
}
