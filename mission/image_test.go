package mission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/resource"
)

// imageBlocks returns the image blocks of a message, so a test can assert what
// pictures a turn carried without caring about block order beyond kind.
func imageBlocks(m llm.Message) []llm.Image {
	var out []llm.Image
	for _, b := range m.Blocks {
		if b.Kind == llm.KindImage && b.Image != nil {
			out = append(out, *b.Image)
		}
	}
	return out
}

// TestUserTurnBuildsTextThenImages proves the shared turn builder puts the
// prose first and one image block per attachment after it, and that an
// image-only turn omits the empty text block rather than sending a blank one.
func TestUserTurnBuildsTextThenImages(t *testing.T) {
	png := llm.Image{MediaType: "image/png", Data: []byte("PNG")}

	withText := userTurn("what is this", []llm.Image{png})
	if len(withText.Blocks) != 2 {
		t.Fatalf("blocks = %d, want text then image", len(withText.Blocks))
	}
	if withText.Blocks[0].Kind != llm.KindText || withText.Blocks[0].Text != "what is this" {
		t.Fatalf("first block = %+v, want the prompt text", withText.Blocks[0])
	}
	if imgs := imageBlocks(withText); len(imgs) != 1 || string(imgs[0].Data) != "PNG" {
		t.Fatalf("image blocks = %+v, want the one attachment", imgs)
	}

	imageOnly := userTurn("", []llm.Image{png})
	if len(imageOnly.Blocks) != 1 || imageOnly.Blocks[0].Kind != llm.KindImage {
		t.Fatalf("image-only turn = %+v, want a single image block", imageOnly.Blocks)
	}
}

// TestContinueConversationCarriesImages proves a reopened turn appends the
// user's images alongside the new line, so a picture pasted mid-conversation
// reaches the next drive.
func TestContinueConversationCarriesImages(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("first answer"))
	exec := NewExecutor(model, WithSystem("sys"))
	_, _, raw := driveToDone(t, exec, 5)

	png := llm.Image{MediaType: "image/png", Data: []byte("shot")}
	reopened, err := ContinueConversation(goal.Status{Checkpoint: raw}, "explain this", png)
	if err != nil {
		t.Fatalf("ContinueConversation: %v", err)
	}
	cp, err := decodeCheckpoint(reopened.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	last := cp.Messages[len(cp.Messages)-1]
	if last.Role != llm.RoleUser || last.TextContent() != "explain this" {
		t.Fatalf("last message = %+v, want the new user line", last)
	}
	if imgs := imageBlocks(last); len(imgs) != 1 || string(imgs[0].Data) != "shot" {
		t.Fatalf("last message images = %+v, want the pasted image", imgs)
	}
}

// TestOpeningTurnCarriesSpecImages proves a goal that opens with attachments
// hands the model an image block on the very first turn, so an image pasted
// before the first send is not lost.
func TestOpeningTurnCarriesSpecImages(t *testing.T) {
	model := llmtest.NewScripted(llmtest.SayText("done"))
	exec := NewExecutor(model, WithSystem("sys"))

	spec := goal.Spec{
		Objective:     "what is in this screenshot",
		StopCondition: "it is done",
		Attachments:   []llm.Image{{MediaType: "image/png", Data: []byte("open")}},
	}
	encSpec, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	r := resource.Resource{APIVersion: goal.GroupVersion, Kind: goal.Kind, Name: "g", Spec: encSpec}
	if _, err := exec.Execute(context.Background(), r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	reqs := model.Requests()
	if len(reqs) == 0 {
		t.Fatal("model saw no request")
	}
	opening := reqs[0].Messages[0]
	if imgs := imageBlocks(opening); len(imgs) != 1 || string(imgs[0].Data) != "open" {
		t.Fatalf("opening message images = %+v, want the seeded attachment", imgs)
	}
}
