package events

import (
	"sync"
	"sync/atomic"
)

// SubscribedEvent is delivered to subscribers. Fields is built once per Emit
// and shared read-only across subscribers; do not mutate it.
type SubscribedEvent struct {
	ID     EventID
	Seq    uint64
	Fields map[string]any
}

// Subscription receives events whose ID matches its filter.
type Subscription struct {
	ch     chan SubscribedEvent
	filter func(EventID) bool
}

// Ch returns the receive-only channel for this subscription.
func (s *Subscription) Ch() <-chan SubscribedEvent {
	return s.ch
}

var (
	subsMu    sync.RWMutex
	subs      []*Subscription
	globalSeq atomic.Uint64
)

// Subscribe registers a subscription receiving events matching filter. Delivery
// is non-blocking: a slow consumer drops events, never backpressuring Emit. The
// caller must invoke the returned unsubscribe function when done (t.Cleanup in
// tests). This is the seam for a future SSE endpoint; nothing subscribes in M1.
func Subscribe(bufSize int, filter func(EventID) bool) (*Subscription, func()) {
	s := &Subscription{
		ch:     make(chan SubscribedEvent, bufSize),
		filter: filter,
	}

	subsMu.Lock()

	subs = append(subs, s)
	subsMu.Unlock()

	unsub := func() {
		subsMu.Lock()
		defer subsMu.Unlock()

		for i, sub := range subs {
			if sub == s {
				subs = append(subs[:i], subs[i+1:]...)

				return
			}
		}
	}

	return s, unsub
}

// fanOutToSubscribers delivers an event to matching subscribers. The Fields map
// is allocated only when at least one subscriber matches, so an idle stream
// costs nothing.
func fanOutToSubscribers(id EventID, dataKvs []any) {
	subsMu.RLock()
	defer subsMu.RUnlock()

	if len(subs) == 0 {
		return
	}

	matched := false

	for _, s := range subs {
		if s.filter(id) {
			matched = true

			break
		}
	}

	if !matched {
		return
	}

	event := SubscribedEvent{
		ID:     id,
		Seq:    globalSeq.Add(1),
		Fields: kvPairsToMap(dataKvs),
	}

	for _, s := range subs {
		if s.filter(id) {
			select {
			case s.ch <- event:
			default: // drop — slow consumer
			}
		}
	}
}

// kvPairsToMap converts alternating key/value pairs to a map, skipping
// non-string keys and any trailing odd element.
func kvPairsToMap(kvs []any) map[string]any {
	m := make(map[string]any, len(kvs)/2)

	for i := 0; i+1 < len(kvs); i += 2 {
		key, ok := kvs[i].(string)
		if !ok {
			continue
		}

		m[key] = kvs[i+1]
	}

	return m
}
