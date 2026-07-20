package dhcpwindows

import (
	"sync"
)

// fingerprint is the set of attributes whose change suggests a wadaptID now
// denotes a *different* scope rather than an edited one.
//
// Name and the addressing are in because a scope recreated on a reused subnet
// is overwhelmingly likely to differ in them — a new tenant names it something
// else and gives it a different range within the subnet. LeaseDuration is in
// because it is the attribute an operator most often sets deliberately per
// scope, so a change is a signal rather than noise.
//
// Description is deliberately *out*. The identity story's whole point is that
// the ID does not live in the description, so editing or clearing it changes
// nothing — warning on it would train an operator to ignore this event.
//
// State and Type are out for the same reason from the other direction:
// activating or deactivating a scope is routine operation of the *same* scope,
// and a warning on it would fire on ordinary use.
type fingerprint struct {
	name                 string
	startRange           string
	endRange             string
	subnetMask           string
	leaseDurationSeconds int
}

// fingerprintOf reduces a scope to the attributes drift detection compares.
func fingerprintOf(s Scope) fingerprint {
	return fingerprint{
		name:                 s.Name,
		startRange:           s.StartRange,
		endRange:             s.EndRange,
		subnetMask:           s.SubnetMask,
		leaseDurationSeconds: s.LeaseDurationSeconds,
	}
}

// driftReport names one identity whose scope changed materially since it was
// last seen.
type driftReport struct {
	wadaptID string
	scopeID  string
	was      fingerprint
	now      fingerprint
}

// driftLedger remembers the last-seen attributes of every scope, keyed by
// wadaptID, so a subnet that is deleted and recreated can be *noticed*.
//
// This is the one piece of adapter state M3 carries, and it exists because the
// plan's only silent mutating failure needed converting into a logged one. A
// wadaptID is derived from the server name and the subnet, and nothing else —
// the host exposes no creation time and no GUID — so a new scope on a reused
// subnet derives the *same* identity as the one that was deleted. weave then
// binds it to the old object's desired state, with no arm fired and no retry
// loop. Detection does not fix that; nothing can. It makes it visible.
//
// The ledger is in memory and dies with the process. That is honest rather than
// unfortunate: it means a restart cannot report drift it did not observe, and a
// persisted one would need to answer what happens when it disagrees with the
// server, which is a bigger question than this guard is worth. A restart
// re-baselines silently, and the first listing after it establishes truth.
//
// Growth is bounded by the number of distinct subnets the server has ever held
// during one process lifetime — hundreds at DHCP scale, and entries are small.
// A deleted scope's entry is retained deliberately: it is exactly the entry
// needed to notice the subnet being reused later, which is the case this exists
// for.
// The zero value is ready to use, and that is deliberate rather than
// convenient: *Client is constructed directly in tests as well as through
// NewClient, and a ledger that needed a constructor would be nil on exactly the
// paths nobody remembered to update — silently disabling detection instead of
// failing. The map is created on first use, under the same lock that guards it.
type driftLedger struct {
	mu   sync.Mutex
	seen map[string]fingerprint
}

// observe records the current attributes of every scope and returns the
// identities whose attributes changed materially since the last observation.
//
// A wadaptID seen for the first time is recorded and reported as nothing: with
// no prior observation there is no drift to detect, only a baseline to set.
//
// It takes the whole listing rather than one scope at a time so the lock is
// taken once per backend call rather than once per scope.
func (l *driftLedger) observe(scopes []Scope) []driftReport {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.seen == nil {
		l.seen = make(map[string]fingerprint, len(scopes))
	}

	var reports []driftReport

	for i := range scopes {
		current := fingerprintOf(scopes[i])

		previous, known := l.seen[scopes[i].WadaptID]
		if known && previous != current {
			reports = append(reports, driftReport{
				wadaptID: scopes[i].WadaptID,
				scopeID:  scopes[i].ScopeID,
				was:      previous,
				now:      current,
			})
		}

		l.seen[scopes[i].WadaptID] = current
	}

	return reports
}
