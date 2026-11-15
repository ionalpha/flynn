// Package stdioguard makes standard output carry protocol frames and nothing else.
//
// A headless transport speaks on stdout, and the process also runs code that writes to
// stdout unaware: a library that prints, a tool handed [os.Stdout], a child that
// inherited descriptor 1. One such write mid-stream is a corrupt frame, or a
// well-formed one the peer did not expect. [Install] duplicates the original stdout
// into a private writer, [Guard.Frames], then points the process's stdout at a pipe the
// guard drains. Everything written to stdout afterwards, by this process or an
// inheriting child, is counted, sampled and optionally forwarded, and never reaches
// the frame stream.
//
// The private writer is created before descriptor 1 is repointed and is not
// inheritable, so a child handed the standard streams gets the pipe, and a child with
// an explicit handle list gets only what the list names.
//
// # Limits
//
// [Install] runs once per process, before any frame is written and before any child
// that should be diverted starts; earlier output is not intercepted. Stray bytes still
// in the pipe at exit are lost, so a diagnostic that must survive a crash goes to
// stderr directly.
package stdioguard

import (
	"errors"
	"io"
	"sync"
)

// SampleLimit is how many bytes of stray output [Guard.Stats] keeps. The rest are only
// counted, so a runaway writer costs a counter and no memory.
const SampleLimit = 4096

// ErrAlreadyInstalled is returned by a second call to [Install]. Redirecting twice would
// capture the first guard's pipe as if it were the original stdout.
var ErrAlreadyInstalled = errors.New("stdioguard: already installed in this process")

// ErrNoStdout is returned when the process has no standard output to guard.
var ErrNoStdout = errors.New("stdioguard: process has no standard output")

// Option configures [Install].
type Option func(*config)

type config struct {
	onStray func([]byte)
	dest    io.Writer
}

// WithStrayHandler calls fn with each chunk of stray output as it is read. The slice is
// reused after fn returns; copy it to keep it. fn runs on the guard's own goroutine, so a
// slow handler backpressures the stray writers and never the frame stream.
func WithStrayHandler(fn func(chunk []byte)) Option {
	return func(c *config) { c.onStray = fn }
}

// WithStrayDestination forwards stray output to w as well as recording it. There is no
// default: when the host joined stderr to stdout, forwarding to stderr sends the bytes
// back into the frame stream, and no portable check detects the join. A caller that
// knows its stderr is separate can opt in.
func WithStrayDestination(w io.Writer) Option {
	return func(c *config) { c.dest = w }
}

// Stats is what the guard has diverted so far.
type Stats struct {
	// StrayBytes is the total number of bytes diverted.
	StrayBytes int64
	// Sample is the first [SampleLimit] bytes diverted.
	Sample []byte
}

// Guard owns the frame writer and the drain of the diverted stream.
type Guard struct {
	frames io.Writer
	cfg    config

	mu    sync.Mutex
	stats Stats

	done chan struct{}
}

var (
	installMu sync.Mutex
	installed bool
)

// Install diverts the process's standard output and returns the Guard that owns the
// frame writer.
func Install(opts ...Option) (*Guard, error) {
	installMu.Lock()
	defer installMu.Unlock()
	if installed {
		return nil, ErrAlreadyInstalled
	}
	frames, strays, err := redirectStdout()
	if err != nil {
		return nil, err
	}
	installed = true
	return newGuard(frames, strays, opts), nil
}

// newGuard is separate from [Install] so the drain can be tested without repointing the
// test process's stdout.
func newGuard(frames io.Writer, strays io.Reader, opts []Option) *Guard {
	g := &Guard{frames: frames, done: make(chan struct{})}
	for _, o := range opts {
		o(&g.cfg)
	}
	go g.drain(strays)
	return g
}

// Frames returns the writer that reaches the original stdout. Hand it to
// ndjson.NewWriter and nothing else, because each extra writer is another way for bytes
// to reach the stream.
func (g *Guard) Frames() io.Writer { return g.frames }

// Stats reports what has been diverted so far. The returned sample is a copy.
func (g *Guard) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.stats
	s.Sample = append([]byte(nil), g.stats.Sample...)
	return s
}

// drain reads the stray pipe until it closes and ignores a failed destination write,
// because a stalled drain fills the pipe and blocks every writer that thinks it is
// writing to stdout.
func (g *Guard) drain(r io.Reader) {
	defer close(g.done)
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			g.record(buf[:n])
			if g.cfg.onStray != nil {
				g.cfg.onStray(buf[:n])
			}
			if g.cfg.dest != nil {
				_, _ = g.cfg.dest.Write(buf[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

func (g *Guard) record(p []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats.StrayBytes += int64(len(p))
	if room := SampleLimit - len(g.stats.Sample); room > 0 {
		g.stats.Sample = append(g.stats.Sample, p[:min(room, len(p))]...)
	}
}
