package state_test

import (
	"testing"

	"github.com/ionalpha/flynn/state"
)

// TestDecodePostImagesReturnTheDecodedRecord guards the decoders against
// returning the record by value in the same statement that fills it: the
// language does not fix whether the value is read before or after the decode
// call, so such a return can hand back the zero value alongside a nil error.
func TestDecodePostImagesReturnTheDecodedRecord(t *testing.T) {
	payload := map[string]any{
		"session":   map[string]any{"ID": "s1", "Title": "title"},
		"turn":      map[string]any{"ID": "t1", "SessionID": "s1", "Content": "hello"},
		"skill":     map[string]any{"ID": "k1", "Slug": "slug"},
		"item":      map[string]any{"ID": "m1", "Content": "remember"},
		"promotion": map[string]any{"MemoryID": "m1", "Promoted": true},
	}

	if got, err := state.DecodeSession(payload); err != nil || got.ID != "s1" || got.Title != "title" {
		t.Errorf("DecodeSession = %+v, %v", got, err)
	}
	if got, err := state.DecodeTurn(payload); err != nil || got.ID != "t1" || got.Content != "hello" {
		t.Errorf("DecodeTurn = %+v, %v", got, err)
	}
	if got, err := state.DecodeSkill(payload); err != nil || got.ID != "k1" || got.Slug != "slug" {
		t.Errorf("DecodeSkill = %+v, %v", got, err)
	}
	if got, err := state.DecodeMemoryItem(payload); err != nil || got.ID != "m1" || got.Content != "remember" {
		t.Errorf("DecodeMemoryItem = %+v, %v", got, err)
	}
	if got, err := state.DecodeMemoryPromotion(payload); err != nil || got.MemoryID != "m1" || !got.Promoted {
		t.Errorf("DecodeMemoryPromotion = %+v, %v", got, err)
	}
}
