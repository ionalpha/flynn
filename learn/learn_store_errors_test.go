package learn

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/internal/conformtest/faultstate"
	"github.com/ionalpha/flynn/state"
)

// TestCuratorReturnsStoreErrorsWithWhatWasAlreadyStored checks the Curate contract
// that the first store error aborts the call and is returned alongside whatever
// was persisted before it, so a caller can report a partial capture honestly.
func TestCuratorReturnsStoreErrorsWithWhatWasAlreadyStored(t *testing.T) {
	boom := errors.New("store is down")
	ctx := context.Background()
	memoryLesson := Lesson{Kind: LessonMemory, Body: "a fact"}
	skillLesson := Lesson{Kind: LessonSkill, Title: "A Skill", Body: "steps"}

	tests := []struct {
		name         string
		lessons      []Lesson
		failSkills   bool
		failMemories bool
		wantMemories int
		wantSkills   int
	}{
		{"skill upsert fails after a memory was stored", []Lesson{memoryLesson, skillLesson}, true, false, 1, 0},
		{"memory write fails after a skill was stored", []Lesson{skillLesson, memoryLesson}, false, true, 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			skills, memories := newStores(t)
			fs := &faultstate.Skills{SkillStore: skills}
			fm := &faultstate.Memory{MemoryStore: memories}
			if tc.failSkills {
				fs.OnUpsert = func(context.Context, state.SkillStore, state.Skill) (state.Skill, error) {
					return state.Skill{}, boom
				}
			}
			if tc.failMemories {
				fm.OnWrite = func(context.Context, state.MemoryStore, state.MemoryItem) (state.MemoryItem, error) {
					return state.MemoryItem{}, boom
				}
			}
			c := NewCurator(&fakeDistiller{lessons: tc.lessons}, fs, fm)

			captured, err := c.Curate(ctx, convergedOutcome())

			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the store's error", err)
			}
			if len(captured.Memories) != tc.wantMemories || len(captured.Skills) != tc.wantSkills {
				t.Fatalf("captured %d memories and %d skills, want %d and %d",
					len(captured.Memories), len(captured.Skills), tc.wantMemories, tc.wantSkills)
			}
		})
	}
}
