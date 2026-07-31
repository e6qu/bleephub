package bleephub

import (
	"sync"
	"testing"
	"time"
)

func (s *ProjectV2Store) replaceClockNow(clockNow func() time.Time) {
	if s == nil {
		return
	}
	s.clockMu.Lock()
	s.clockNow = clockNow
	s.clockMu.Unlock()
}

func (st *Store) replaceClockNow(clockNow func() time.Time) func() time.Time {
	if st == nil {
		return nil
	}
	st.clockMu.Lock()
	previous := st.clockNow
	st.clockNow = clockNow
	st.clockMu.Unlock()
	if st.ProjectsV2 != nil {
		st.ProjectsV2.replaceClockNow(clockNow)
	}
	return previous
}

// replaceClockNow atomically installs a deterministic test clock and returns
// the previous source so callers can restore it. Servers own concurrent
// schedulers and timeout watchers that may still read the clock during cleanup,
// so test code must never assign clockNow directly.
func (s *Server) replaceClockNow(clockNow func() time.Time) func() time.Time {
	if s == nil {
		return nil
	}
	s.clockMu.Lock()
	previous := s.clockNow
	s.clockNow = clockNow
	s.clockMu.Unlock()
	if s.store != nil {
		s.store.replaceClockNow(clockNow)
	}
	return previous
}

func TestServerClockReplacementIsConcurrentSafe(t *testing.T) {
	first := time.Date(2040, time.January, 2, 3, 4, 5, 0, time.UTC)
	second := first.Add(time.Hour)
	firstClock := func() time.Time { return first }
	secondClock := func() time.Time { return second }

	server := &Server{}
	if previous := server.replaceClockNow(firstClock); previous != nil {
		t.Fatal("initial clock replacement returned a previous clock")
	}

	const iterations = 1_000
	var workers sync.WaitGroup
	workers.Add(5)
	for range 4 {
		go func() {
			defer workers.Done()
			for range iterations {
				got := server.currentTime()
				if got != first && got != second {
					t.Errorf("currentTime() = %s, want one of the injected clocks", got)
					return
				}
			}
		}()
	}
	go func() {
		defer workers.Done()
		for index := range iterations {
			if index%2 == 0 {
				server.replaceClockNow(secondClock)
			} else {
				server.replaceClockNow(firstClock)
			}
		}
	}()
	workers.Wait()

	previous := server.replaceClockNow(nil)
	if previous == nil {
		t.Fatal("clearing the clock did not return the installed source")
	}
	if got := previous(); got != first && got != second {
		t.Fatalf("previous clock returned %s, want one of the injected clocks", got)
	}
}
