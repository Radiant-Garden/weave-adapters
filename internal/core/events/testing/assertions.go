package testing

import (
	"testing"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// AssertEmitted fails the test if no event with the given ID was recorded.
func (r *Recorder) AssertEmitted(t *testing.T, id events.EventID) {
	t.Helper()

	if len(r.FindByID(id)) == 0 {
		t.Errorf("expected event %s to be emitted, but it was not", id)
	}
}

// AssertNotEmitted fails the test if any event with the given ID was recorded.
func (r *Recorder) AssertNotEmitted(t *testing.T, id events.EventID) {
	t.Helper()

	if n := len(r.FindByID(id)); n > 0 {
		t.Errorf("expected event %s not to be emitted, but it was emitted %d time(s)", id, n)
	}
}

// AssertEmittedN fails the test unless the event was recorded exactly n times.
func (r *Recorder) AssertEmittedN(t *testing.T, id events.EventID, n int) {
	t.Helper()

	if got := len(r.FindByID(id)); got != n {
		t.Errorf("expected event %s to be emitted %d time(s), got %d", id, n, got)
	}
}

// AssertData fails the test unless a recorded event with the given ID carried
// the given data field with the expected value.
func (r *Recorder) AssertData(t *testing.T, id events.EventID, key string, want any) {
	t.Helper()

	for _, e := range r.FindByID(id) {
		if e.Data(key) == want {
			return
		}
	}

	t.Errorf("expected event %s to carry data %s=%v, but no recorded event matched", id, key, want)
}
