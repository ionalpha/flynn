package app_test

import (
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/internal/tui/app"
	"github.com/ionalpha/flynn/internal/tui/screen"
)

const eraseScrollback = "\x1b[3J"

// widthBlock shows the width it was laid out at.
func widthBlock(name string) screen.Component {
	return screen.Func(func(w int) []string { return []string{name + "@" + strconv.Itoa(w)} })
}

func TestWidthChangeLaysTheTranscriptOutAgain(t *testing.T) {
	s := start(t, nil)
	s.app.Append(widthBlock("first"))
	s.awaitOutput(t, "first@40")

	before := len(s.out.String())
	s.app.Resize(40, 20)
	s.app.Append(widthBlock("second"))
	s.awaitOutput(t, "second@40")
	if strings.Contains(s.out.String()[before:], eraseScrollback) {
		t.Fatal("a height change erased the scrollback")
	}

	before = len(s.out.String())
	s.app.Resize(25, 20)
	eventually(t, func() bool {
		tail := s.out.String()[before:]
		i := strings.Index(tail, eraseScrollback)
		return i >= 0 && strings.Contains(tail[i:], "first@25\r\nsecond@25\r\n")
	}, func() {
		t.Fatalf("transcript not laid out again; tail:\n%q", s.out.String()[before:])
	})
}

// A reflow lays out the latest 5000 blocks; older ones are no longer kept.
func TestReflowKeepsTheLatestBlocks(t *testing.T) {
	s := start(t, nil)
	blocks := make([]screen.Component, 5001)
	for i := range blocks {
		blocks[i] = widthBlock("block" + strconv.Itoa(i))
	}
	s.app.Append(blocks...)
	s.awaitOutput(t, "block5000@40")

	s.app.Resize(25, 12)
	s.awaitOutput(t, "block5000@25")
	out := s.out.String()
	if strings.Contains(out, "block0@25") || !strings.Contains(out, "block1@25") {
		t.Fatal("the reflow did not keep exactly the latest blocks")
	}
}

// A block appended after the window changed, before anything has reported the
// change, is laid out at the new width.
func TestAppendReadsTheSizeFirst(t *testing.T) {
	var width atomic.Int64
	width.Store(40)
	s := start(t, func(c *app.Config) {
		c.Size = func() (int, int, error) { return int(width.Load()), 12, nil }
	})
	s.app.Append(widthBlock("first"))
	s.awaitOutput(t, "first@40")
	width.Store(31)
	s.app.Append(widthBlock("second"))
	s.awaitOutput(t, "first@31\r\nsecond@31\r\n")
	if strings.Contains(s.out.String(), "second@40") {
		t.Fatal("a block was laid out at the old width")
	}
}
