package notices

import (
	"context"
	"io"
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Client is the whole notice channel as one thing a command can call: it shows what the
// last accepted feed says, and it looks for a newer one when it is time to.
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

// Show prints the notices that apply to this version and have not been said, from the
// cached feed, and marks the one-shot ones as said. It reports whether it printed
// anything.
//
// It never touches the network, so it cannot make a command slower or fail one that would
// otherwise have worked, and it is safe on a machine with no route out. Every failure
// along the way (no cache, unreadable cache, a cached document that no longer verifies) is
// simply no notices: this is a channel for telling the user things, and it has no business
// turning into an error that stops the work they actually asked for.
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
	// A failure to record what was shown means a notice may be shown twice. That is the
	// harmless direction, so it is not worth failing anything over.
	if tr, err := c.Store.MarkShown(tr, pending); err == nil {
		_ = tr
	}
	return wrote
}

// Floors returns the runtime version gates the last accepted feed carries, or nothing when
// there is no trusted feed. The caller applies them (see inference.Raise), and applying
// them can only ever tighten a gate: this package deliberately does not import the
// inference package, so there is no path here that could reach in and relax one.
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

// RefreshIfDue fetches a new feed when the refresh interval has elapsed, and does nothing
// otherwise. It is meant to be called in the background: what it fetches is shown by the
// next run, not this one, which is what keeps the notice channel off the path of the
// command the user is waiting for.
//
// An error is returned for a caller that wants to report it (`flynn notices --refresh`
// does). The background caller ignores it: a notice check that failed is not something to
// interrupt a user's run about, and the next run tries again.
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

// Background starts the refresh in a goroutine and returns immediately, so a command pays
// nothing for the notice channel: not the DNS lookup, not the connection, not the read. If
// the process exits before the fetch finishes, nothing is lost. The feed was already going
// to be shown on the next run, and the next run will try again.
func (c *Client) Background(ctx context.Context) {
	if !enabled(c.Ring) {
		return
	}
	go func() {
		// The fetch is detached from the command's context on purpose: cancelling the
		// user's work should not be the thing that decides whether we ever hear about a
		// vulnerability, and the fetch bounds itself with FetchTimeout anyway.
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), FetchTimeout+time.Second)
		defer cancel()
		_ = c.RefreshIfDue(bg)
	}()
}
