package term

import (
	"context"
	"sync"
	"time"

	"golang.design/x/clipboard"
)

// Clipboard reads images from the OS clipboard for Ctrl+V paste. It is read only
// on the user's keystroke, never the agent's initiative, so it lives here beside
// the $EDITOR handoff where no engine can reach it. A host without a clipboard
// (a pipe, headless CI) degrades to no image.
type Clipboard interface {
	// Image returns the clipboard's image as PNG, or ok=false when there is none
	// or the clipboard is unavailable.
	Image() (data []byte, ok bool)
}

// osClipboard is backed by golang.design/x/clipboard, which is Cgo-free on every
// desktop OS, preserving the single static binary.
type osClipboard struct {
	// init and read are the library's Init and Read, replaceable in tests.
	init func() error
	read func(context.Context, clipboard.Format, ...clipboard.Option) ([]byte, error)

	once  sync.Once
	ready bool
}

// clipboardReadTimeout bounds one paste read. On X11 an owner that never answers
// would otherwise freeze the composer for the library's five-second ceiling.
const clipboardReadTimeout = 2 * time.Second

// NewClipboard returns the OS clipboard. Nothing is probed until the first read,
// so it is safe on a host with no display.
func NewClipboard() Clipboard {
	return &osClipboard{init: clipboard.Init, read: clipboard.Read}
}

// Image initializes the backend once and caches whether it is usable, so a host
// whose Init failed reports no image instead of retrying on every keystroke.
func (c *osClipboard) Image() ([]byte, bool) {
	c.once.Do(func() { c.ready = c.init() == nil })
	if !c.ready {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), clipboardReadTimeout)
	defer cancel()
	data, err := c.read(ctx, clipboard.FmtImage)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

// ImagePNG is the media type of every clipboard image; the library normalizes to PNG.
const ImagePNG = "image/png"
