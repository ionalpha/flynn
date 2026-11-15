package screen

import (
	"time"

	"github.com/ionalpha/flynn/clock"
)

// Scheduler coalesces repaint requests into frames at a bounded rate. Producers
// call Request on any change; paint runs soon after the first request and then at
// most once per interval, so streaming costs one repaint per frame, not per
// token.
//
// Time comes from the injected clock.Timing, so tests drive a clock.Manual and
// observe exact coalescing with no sleeps.
type Scheduler struct {
	requests chan struct{}
	stop     chan struct{}
	done     chan struct{}
}

// NewScheduler starts a scheduler that invokes paint on its own goroutine.
// interval is the minimum time between paint starts; requests inside it coalesce
// into one trailing paint. Stop releases the goroutine.
func NewScheduler(timing clock.Timing, interval time.Duration, paint func()) *Scheduler {
	s := &Scheduler{
		requests: make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go s.run(timing, interval, paint)
	return s
}

// Request asks for a repaint. It never blocks and is safe from any goroutine.
func (s *Scheduler) Request() {
	select {
	case s.requests <- struct{}{}:
	default:
	}
}

// Stop waits for the paint goroutine to exit, so no paint runs after it returns
// and the terminal is safe to restore.
func (s *Scheduler) Stop() {
	close(s.stop)
	<-s.done
}

func (s *Scheduler) run(timing clock.Timing, interval time.Duration, paint func()) {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case <-s.requests:
		}
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
