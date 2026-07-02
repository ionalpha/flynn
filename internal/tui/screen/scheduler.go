package screen

import (
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Scheduler coalesces repaint requests into frames at a bounded rate. Every
// producer (a streaming token, a spinner tick, a keystroke echo) calls
// Request whenever anything changes; the scheduler guarantees the paint
// function runs soon after the first request and then at most once per
// interval, however many requests arrive in between. Fast streaming
// therefore costs one repaint per frame, not one per token, and the renderer
// can never fall behind its input.
//
// Time comes from the injected clock.Timing, never the wall clock, keeping
// frame pacing on the same deterministic footing as the rest of the runtime:
// production wires clock.System, tests drive a clock.Manual and observe
// exact coalescing behavior with no sleeps.
type Scheduler struct {
	requests chan struct{}
	stop     chan struct{}
	done     chan struct{}
}

// NewScheduler starts a scheduler that invokes paint on its own goroutine.
// The interval is the minimum time between the start of one paint and the
// next; requests inside the interval coalesce into at most one trailing
// paint. Stop must be called to release the goroutine.
func NewScheduler(timing clock.Timing, interval time.Duration, paint func()) *Scheduler {
	s := &Scheduler{
		requests: make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go s.run(timing, interval, paint)
	return s
}

// Request asks for a repaint. It never blocks and is safe from any
// goroutine; a request during an in-flight or cooling-down frame folds into
// the next one.
func (s *Scheduler) Request() {
	select {
	case s.requests <- struct{}{}:
	default:
	}
}

// Stop shuts the scheduler down and waits for the paint goroutine to exit,
// so the caller knows no paint can run after Stop returns (safe to restore
// the terminal).
func (s *Scheduler) Stop() {
	close(s.stop)
	<-s.done
}

func (s *Scheduler) run(timing clock.Timing, interval time.Duration, paint func()) {
	defer close(s.done)
	for {
		// Idle: wait for the first request.
		select {
		case <-s.stop:
			return
		case <-s.requests:
		}
		// Paint, then hold for the interval, folding any requests that
		// arrive meanwhile into one trailing paint.
		for {
			paint()
			timer := timing.NewTimer(interval)
			pending := false
			cooling := true
			for cooling {
				select {
				case <-s.stop:
					timer.Stop()
					return
				case <-s.requests:
					pending = true
				case <-timer.C():
					cooling = false
				}
			}
			if !pending {
				break
			}
		}
	}
}
