package notices

import (
	"context"
	"io"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Client is the notice channel as a command uses it: show the cached feed, refresh it
// when due.
type Client struct {
	Source  Source
	Ring    *Keyring
	Store   *Store
	Clock   clock.Clock
	Version string // the running Flynn version, for deciding which notices apply
}

// NewClient builds the production client for a data directory and running version.
func NewClient(dataDir, flynnVersion string) *Client {
	return &Client{
		Source:  Source{URL: DefaultURL},
		Ring:    DefaultKeyring(),
		Store:   NewStore(dataDir),
		Clock:   clock.System{},
		Version: flynnVersion,
	}
}

// Show prints the pending notices from the cached feed, marks the one-shot ones shown,
// and reports whether it printed anything. It never touches the network, and every
// failure (no cache, an unreadable one, one that no longer verifies) means no notices,
// so it cannot slow or fail the command.
func (c *Client) Show(w io.Writer) bool {
	if !enabled(c.Ring) {
		return false
	}
	f, tr, ok := Cached(c.Store, c.Ring)
	if !ok {
		return false
	}
	pending := Pending(f, c.Version, tr)
	stale := f.Stale(c.Clock.Now())
	if len(pending) == 0 && !stale {
		return false
	}
	wrote := Render(w, pending, stale)
	// A failed write only means a notice may be shown twice.
	if tr, err := c.Store.MarkShown(tr, pending); err == nil {
		_ = tr
	}
	return wrote
}

// Floors returns the runtime version gates in the cached feed, or nil. The caller
// applies them with inference.Raise, which can only tighten a gate; this package does
// not import inference, so it has no way to relax one.
func (c *Client) Floors() []Floor {
	if !enabled(c.Ring) {
		return nil
	}
	f, _, ok := Cached(c.Store, c.Ring)
	if !ok {
		return nil
	}
	return f.Floors
}

// RefreshIfDue fetches a new feed when [Due]. What it fetches is shown by the next run,
// which keeps the fetch off the command's path. `flynn notices --refresh` reports the
// error; [Client.Background] ignores it and the next run tries again.
func (c *Client) RefreshIfDue(ctx context.Context) error {
	if !enabled(c.Ring) {
		return nil
	}
	tr, err := c.Store.LoadTrust()
	if err != nil {
		return err
	}
	if !Due(tr, c.Clock.Now()) {
		return nil
	}
	return c.Refresh(ctx)
}

// Refresh fetches and accepts a feed now, regardless of the interval.
func (c *Client) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	_, err := Refresh(ctx, c.Source, c.Ring, c.Store, c.Clock.Now())
	return err
}

// Background runs [Client.RefreshIfDue] in a goroutine, so a command waits on no DNS,
// connection or read. A process that exits first loses nothing; the next run retries.
func (c *Client) Background(ctx context.Context) {
	if !enabled(c.Ring) {
		return
	}
	go func() {
		// Detached from ctx so cancelling the command does not cancel the advisory
		// fetch; FetchTimeout bounds it.
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), FetchTimeout+time.Second)
		defer cancel()
		_ = c.RefreshIfDue(bg)
	}()
}
