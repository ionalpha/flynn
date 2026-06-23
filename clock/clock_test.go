package clock_test

import (
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
)

func TestSystemNowIsUTC(t *testing.T) {
	if loc := (clock.System{}).Now().Location(); loc != time.UTC {
		t.Fatalf("System.Now location = %v, want UTC", loc)
	}
}

func TestManualAdvanceAndSet(t *testing.T) {
	start := time.Unix(100, 0).UTC()
	m := clock.NewManual(start)

	if !m.Now().Equal(start) {
		t.Fatalf("initial Now = %v, want %v", m.Now(), start)
	}

	m.Advance(5 * time.Second)
	if want := start.Add(5 * time.Second); !m.Now().Equal(want) {
		t.Fatalf("after Advance Now = %v, want %v", m.Now(), want)
	}

	// Set normalises to UTC and replaces the time outright.
	m.Set(time.Unix(500, 0))
	if want := time.Unix(500, 0).UTC(); !m.Now().Equal(want) {
		t.Fatalf("after Set Now = %v, want %v", m.Now(), want)
	}
}
