package main

import (
	"strings"
	"testing"
)

// TestCommandCompleteNames proves a partial command word offers the matching commands,
// and that a command taking an argument applies with a trailing space so the argument
// can be typed next.
func TestCommandCompleteNames(t *testing.T) {
	c := newCommandCompleter()
	got := c.Suggest("/mo")
	if len(got) == 0 {
		t.Fatal("/mo suggested no commands")
	}
	var names []string
	for _, x := range got {
		names = append(names, x.Show)
		if x.Show == "/model" && x.Apply != "/model " {
			t.Fatalf("/model apply = %q, want %q", x.Apply, "/model ")
		}
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "/model") || !strings.Contains(joined, "/models") {
		t.Fatalf("/mo did not suggest /model and /models: %v", names)
	}
}

// TestCommandCompleteExactCommandSuppressed proves a fully typed command offers no
// completion, so Enter submits it rather than reopening a menu.
func TestCommandCompleteExactCommandSuppressed(t *testing.T) {
	c := newCommandCompleter()
	if got := c.Suggest("/models"); got != nil {
		t.Fatalf("a fully typed command should not complete, got %v", got)
	}
}

// TestCommandCompleteModelArg proves /model completes an argument against the catalog:
// a bare "/model " offers the models, a partial narrows them, and each candidate applies
// as a full "/model <id>" line.
func TestCommandCompleteModelArg(t *testing.T) {
	c := newCommandCompleter()
	if len(c.models) == 0 {
		t.Skip("catalog has no models in this build")
	}
	if got := c.Suggest("/model "); len(got) == 0 {
		t.Fatal("/model with a space offered no models")
	}
	got := c.Suggest("/model gpt")
	if len(got) == 0 {
		t.Fatal("/model gpt matched no model")
	}
	for _, x := range got {
		if !strings.HasPrefix(x.Apply, "/model ") {
			t.Fatalf("model candidate apply = %q, want a /model line", x.Apply)
		}
	}
}

// TestCommandCompleteNonCommand proves a normal message is not treated as a command.
func TestCommandCompleteNonCommand(t *testing.T) {
	c := newCommandCompleter()
	if got := c.Suggest("hello world"); got != nil {
		t.Fatalf("a normal message should not complete, got %v", got)
	}
}
