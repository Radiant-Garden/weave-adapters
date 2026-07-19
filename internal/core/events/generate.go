package events

// The event catalog reference (docs/events.md) is generated from the registry.
// `task generate` runs this; CI's generate-check fails if it is out of date.
//
// The generator lives in internal/docgen, outside core, because it must import
// every adapter's event package to see their events — and core must never
// import internal/adapters. The directive stays here, next to the registry it
// documents; a go:generate comment creates no import, so it carries no
// dependency of its own.
//go:generate go run ../../docgen
