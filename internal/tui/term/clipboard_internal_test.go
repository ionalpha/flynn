package term

import (
	"context"
	"errors"
	"testing"

	"golang.design/x/clipboard"
)

func TestOSClipboardImage(t *testing.T) {
	png := []byte("\x89PNG")
	cases := []struct {
		name    string
		initErr error
		data    []byte
		readErr error
		want    []byte
		wantOK  bool
	}{
		{name: "an image on the clipboard is returned", data: png, want: png, wantOK: true},
		{name: "no clipboard on this host", initErr: errors.New("no display")},
		{name: "a read that fails or times out", readErr: context.DeadlineExceeded},
		{name: "the clipboard holds no image", data: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			c := &osClipboard{
				init: func() error { return tc.initErr },
				read: func(ctx context.Context, f clipboard.Format, _ ...clipboard.Option) ([]byte, error) {
					reads++
					if _, ok := ctx.Deadline(); !ok {
						t.Error("read was given no deadline")
					}
					if f != clipboard.FmtImage {
						t.Errorf("read format = %v, want FmtImage", f)
					}
					return tc.data, tc.readErr
				},
			}
			got, ok := c.Image()
			if ok != tc.wantOK || string(got) != string(tc.want) {
				t.Fatalf("Image() = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
			if tc.initErr != nil && reads != 0 {
				t.Fatalf("read %d times after Init failed, want 0", reads)
			}
		})
	}
}

// TestNewClipboardIsWiredToTheLibrary checks construction only: it must not
// touch the clipboard, so the suite stays safe on a host with no display.
func TestNewClipboardIsWiredToTheLibrary(t *testing.T) {
	c, ok := NewClipboard().(*osClipboard)
	if !ok || c.init == nil || c.read == nil {
		t.Fatalf("NewClipboard() = %#v, want an osClipboard with init and read set", c)
	}
}

func TestOSClipboardInitsOnce(t *testing.T) {
	inits := 0
	c := &osClipboard{
		init: func() error { inits++; return errors.New("no display") },
		read: func(context.Context, clipboard.Format, ...clipboard.Option) ([]byte, error) {
			return nil, nil
		},
	}
	c.Image()
	c.Image()
	if inits != 1 {
		t.Fatalf("Init ran %d times, want 1", inits)
	}
}
