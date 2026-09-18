//go:build !windows

package winsvc

import (
	"time"
)

// IsService always reports false off Windows.
//
// It exists so the binary's wiring compiles and runs unchanged on a developer
// machine: the console path is selected by this returning false, not by a build
// tag at the call site.
func IsService() (bool, error) { return false, nil }

// Run always fails off Windows.
//
// It returns an error rather than falling back to running serve directly.
// Silently serving would make a mistaken call look like it worked, and the one
// caller reaches this only after IsService reported true — which cannot happen
// here.
func Run(_ string, _ time.Duration, _ ServeFunc) error { return ErrUnsupported }
