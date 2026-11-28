package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/internal/text"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
)

// recordGrants returns a context under which each wait on a sandbox grant is
// recorded on sess and then passed to show, which may be nil.
func recordGrants(ctx context.Context, sess *session.Session, show func(sandbox.GrantProgress)) context.Context {
	return sandbox.WithGrantProgress(ctx, grantReporter(ctx, sess, show))
}

func grantReporter(ctx context.Context, sess *session.Session, show func(sandbox.GrantProgress)) func(sandbox.GrantProgress) {
	return func(p sandbox.GrantProgress) {
		if p.First || p.Done {
			g := session.SandboxGrant{Dir: p.Dir, State: grantState(p), ElapsedMS: p.Elapsed.Milliseconds()}
			reason := ""
			if g.State == session.GrantFailed {
				reason = p.Err.Error()
			}
			sess.RecordGrant(ctx, g, reason)
		}
		if show != nil {
			show(p)
		}
	}
}

func grantState(p sandbox.GrantProgress) session.GrantState {
	switch {
	case !p.Done:
		return session.GrantStarted
	case p.Err == nil:
		return session.GrantFinished
	case errors.Is(p.Err, context.Canceled), errors.Is(p.Err, context.DeadlineExceeded):
		return session.GrantCancelled
	}
	return session.GrantFailed
}

// grantLine is the transcript line for a recorded wait on a sandbox grant. The
// directory is written in full, since the record is where a person checks it.
func grantLine(ev session.Event) string {
	g := ev.SandboxGrant
	if g == nil {
		return "sandbox access: unreadable record"
	}
	waited := g.Elapsed().Round(100 * time.Millisecond).String()
	line := "sandbox access " + string(g.State) + " after " + waited + ": " + g.Dir
	if g.State == session.GrantStarted {
		line = "sandbox access started, waited " + waited + " so far: " + g.Dir
	}
	if ev.Err != "" {
		line += " (" + ev.Err + ")"
	}
	return text.Inert(line)
}

// grantRepeatEvery is how often a run with no status line repeats that it is
// still waiting.
const grantRepeatEvery = 10 * time.Second

// printGrantProgress writes a line when a wait on a grant is first reported and
// every [grantRepeatEvery] after, for a run that only prints.
func printGrantProgress(w io.Writer) func(sandbox.GrantProgress) {
	var mu sync.Mutex
	said := map[string]time.Duration{}
	return func(p sandbox.GrantProgress) {
		mu.Lock()
		defer mu.Unlock()
		if p.Done {
			delete(said, p.Dir)
			return
		}
		if last, ok := said[p.Dir]; ok && p.Elapsed-last < grantRepeatEvery {
			return
		}
		said[p.Dir] = p.Elapsed
		_, _ = fmt.Fprintln(w, grantStatusText(p, "Ctrl-C cancels", 0))
	}
}

// grantAhead starts the session's directory grant in the background and returns
// a stop that ends the wait for it. A failure is left for the first command to
// report, which makes the same grant. Stopping does not interrupt the rewrite
// itself, which Windows cannot do.
func grantAhead(ctx context.Context, shell governed.ShellPolicy, workdir string) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = shell.GrantWorkdir(ctx, workdir)
	}()
	return func() {
		cancel()
		<-done
	}
}
