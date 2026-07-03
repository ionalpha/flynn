package term

import (
	"sync"

	"golang.design/x/clipboard"
)

// Clipboard reads image content from the OS clipboard. It is the port behind
// Ctrl+V image paste: reading the user's clipboard happens only at their
// explicit keystroke, never on the agent's initiative, so it lives here beside
// the $EDITOR handoff rather than anywhere an engine can reach. The interface
// keeps the composer testable with a fake and lets a host that has no
// clipboard (a pipe, a headless CI run) degrade cleanly instead of failing.
type Clipboard interface {
	// Image returns a PNG-encoded image on the clipboard and ok=true, or
	// ok=false when the clipboard holds no image or is unavailable on this
	// host or session. The returned bytes are always image/png.
	Image() (data []byte, ok bool)
}

// osClipboard is the real clipboard backed by golang.design/x/clipboard. That
// library is Cgo-free on every desktop OS (purego on macOS, syscall on
// Windows, pure-Go X11 and Wayland on Linux), so it preserves flynn's
// single-static-binary property. Read(FmtImage) always returns canonical PNG.
type osClipboard struct {
	once  sync.Once
	ready bool
}

// NewClipboard returns the OS clipboard port. It does not touch the clipboard
// or probe availability until the first read, so constructing it is free and
// safe on a host that has no display.
func NewClipboard() Clipboard { return &osClipboard{} }

// Image reads a PNG image off the clipboard. The first call initializes the
// backend once and caches whether it is usable; a host without a clipboard
// (Init failed) reports no image forever after rather than retrying on every
// keystroke.
func (c *osClipboard) Image() ([]byte, bool) {
	c.once.Do(func() { c.ready = clipboard.Init() == nil })
	if !c.ready {
		return nil, false
	}
	data := clipboard.Read(clipboard.FmtImage)
	if len(data) == 0 {
		return nil, false
	}
	return data, true
}

// ImagePNG is the media type every image the clipboard port yields carries:
// golang.design/x/clipboard normalizes clipboard bitmaps to PNG on read.
const ImagePNG = "image/png"
