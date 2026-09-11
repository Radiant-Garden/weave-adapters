//go:build !windows

package winsvc

import (
	"io"
	"log/slog"
)

// NewEventLogHandler always fails off Windows.
//
// The binary reaches it only after IsService reported true, which cannot happen
// here, so an error is the honest answer rather than a handler that discards.
func NewEventLogHandler(string) (slog.Handler, io.Closer, error) { return nil, nil, ErrUnsupported }

// InstallEventLogSource always fails off Windows.
func InstallEventLogSource(string) error { return ErrUnsupported }

// RemoveEventLogSource always fails off Windows.
func RemoveEventLogSource(string) error { return ErrUnsupported }
