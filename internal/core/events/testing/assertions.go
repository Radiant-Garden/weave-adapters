package testing

import (
	"fmt"
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

// AssertMatchesCatalog fails the test for every recorded event that diverges
// from its catalog spec: an unregistered ID, a missing required field, or a
// data field the catalog does not declare. This guards against the
// catalog-drift anti-pattern — an event whose emitted fields no longer match
// docs/events.md.
func (r *Recorder) AssertMatchesCatalog(t *testing.T) {
	t.Helper()

	for _, err := range r.matchCatalog() {
		t.Error(err)
	}
}

// matchCatalog returns one error per catalog-conformance problem across all
// recorded events. A required field is satisfied by any captured group (data,
// caller, or request), since Emit places caller identity in its own groups;
// the undeclared-key check is scoped to the data group, which is the only one
// the emitting caller controls.
func (r *Recorder) matchCatalog() []error {
	var problems []error

	for _, e := range r.All() {
		if e.Spec == nil {
			problems = append(problems, fmt.Errorf("event %s is not registered in the catalog", e.ID))

			continue
		}

		declared := make(map[string]bool, len(e.Spec.Fields))
		for _, f := range e.Spec.Fields {
			declared[f.Name] = true
		}

		for _, f := range e.Spec.Fields {
			if f.Required && !e.has("data", f.Name) && !e.has("caller", f.Name) && !e.has("request", f.Name) {
				problems = append(problems, fmt.Errorf("event %s is missing required field %q", e.ID, f.Name))
			}
		}

		for key := range e.Groups["data"] {
			if !declared[key] {
				problems = append(problems, fmt.Errorf("event %s carries undeclared data field %q", e.ID, key))
			}
		}
	}

	return problems
}
