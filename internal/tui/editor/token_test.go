package editor

import "testing"

func TestTokenActivatesOnTriggerWord(t *testing.T) {
	var e Editor
	e.Insert("see @src/f")
	q, ok := e.Token('@')
	if !ok || q != "src/f" {
		t.Fatalf("Token = %q, %v; want \"src/f\", true", q, ok)
	}
}

func TestTokenBareTriggerHasEmptyQuery(t *testing.T) {
	var e Editor
	e.Insert("look at @")
	q, ok := e.Token('@')
	if !ok || q != "" {
		t.Fatalf("Token = %q, %v; want \"\", true", q, ok)
	}
}

func TestTokenIgnoresTriggerInsideAWord(t *testing.T) {
	var e Editor
	e.Insert("mail user@host")
	if _, ok := e.Token('@'); ok {
		t.Fatal("mid-word trigger activated completion")
	}
}

func TestTokenInactiveWithoutTrigger(t *testing.T) {
	var e Editor
	e.Insert("plain words")
	if _, ok := e.Token('@'); ok {
		t.Fatal("token active with no trigger present")
	}
	e.Clear()
	if _, ok := e.Token('@'); ok {
		t.Fatal("token active on an empty buffer")
	}
}

func TestTokenEndsAtTheCursor(t *testing.T) {
	var e Editor
	e.Insert("@abc")
	e.Left()
	q, ok := e.Token('@')
	if !ok || q != "ab" {
		t.Fatalf("Token = %q, %v; want \"ab\", true", q, ok)
	}
	e.LineStart()
	if _, ok := e.Token('@'); ok {
		t.Fatal("token active with the cursor before the trigger")
	}
}

func TestTokenStopsAtLineBreaksAndChips(t *testing.T) {
	var e Editor
	e.Insert("@top\nnext")
	if _, ok := e.Token('@'); ok {
		t.Fatal("token crossed a line break")
	}
	e.Clear()
	e.InsertPaste("one\ntwo") // becomes a chip
	e.Insert("tail")
	if _, ok := e.Token('@'); ok {
		t.Fatal("token crossed a chip")
	}
}

func TestCompleteTokenReplacesQueryAndAppendsSpace(t *testing.T) {
	var e Editor
	e.Insert("read @sr")
	e.CompleteToken('@', "screen/render.go")
	if got := e.Content(); got != "read @screen/render.go " {
		t.Fatalf("Content = %q", got)
	}
	// The cursor sits after the trailing space: typing continues the prompt.
	e.Insert("x")
	if got := e.Content(); got != "read @screen/render.go x" {
		t.Fatalf("Content after typing = %q", got)
	}
}

func TestCompleteTokenKeepsTextAfterTheCursor(t *testing.T) {
	var e Editor
	e.Insert("@sr tail")
	for range " tail" {
		e.Left()
	}
	e.CompleteToken('@', "src/main.go")
	if got := e.Content(); got != "@src/main.go  tail" {
		t.Fatalf("Content = %q", got)
	}
}

func TestCompleteTokenWithoutActiveTokenDoesNothing(t *testing.T) {
	var e Editor
	e.Insert("plain")
	e.CompleteToken('@', "src/main.go")
	if got := e.Content(); got != "plain" {
		t.Fatalf("Content = %q; buffer changed with no active token", got)
	}
}

func TestCompleteTokenIsOneUndoStep(t *testing.T) {
	var e Editor
	e.Insert("@sr")
	e.CompleteToken('@', "screen/render.go")
	if !e.Undo() {
		t.Fatal("Undo reported nothing to undo")
	}
	if got := e.Content(); got != "@sr" {
		t.Fatalf("Content after undo = %q; want the typed query back", got)
	}
}
