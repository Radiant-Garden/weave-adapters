// Package catalogs registers every event catalog in this module, so anything
// that needs to see the *whole* registry can get it from one import.
//
// Events register from init(), so a walker only sees the catalogs its own
// binary happens to link. Two things need all of them and neither is a normal
// consumer: internal/docgen, which renders docs/events.md, and the conformance
// test that holds api/common/errors.yaml's ProblemType enum to the codes some
// event actually emits. Both were importing the catalogs themselves, which made
// "a new adapter must add a blank import" true in two places instead of one —
// and the failure is silent in both. A missing catalog leaves its events out of
// the reference while generate-check still passes, and leaves its response
// codes looking like ghost entries in the enum.
//
// So the list lives here, once. **A new adapter adds its line below and nowhere
// else.**
//
// This package is deliberately outside internal/core: it imports
// internal/adapters, which core must never do. It is wiring for build tools and
// tests rather than part of the reusable core, so it may see both halves.
package catalogs

import (
	// Blank imports so every init() has registered before anything walks the
	// registry. One per catalog: core's, then each adapter's.
	_ "github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows/events"
	_ "github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)
