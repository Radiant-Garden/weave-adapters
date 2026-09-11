//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"io"
	"log/slog"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/eventlog"
)

// eventLogKey is where the Application log's sources live. eventlog does not
// export it, and sourceRegistered needs to look one up.
const eventLogKey = `SYSTEM\CurrentControlSet\Services\EventLog\Application`

// errSourceNotFound is the "no such registry key" Windows returns for an event
// log source that was already removed.
var errSourceNotFound = windows.ERROR_FILE_NOT_FOUND

// realEventLog writes through the Windows Event Log API.
type realEventLog struct{ l *eventlog.Log }

func (r realEventLog) info(eid uint32, msg string) error    { return r.l.Info(eid, msg) }
func (r realEventLog) warning(eid uint32, msg string) error { return r.l.Warning(eid, msg) }
func (r realEventLog) error(eid uint32, msg string) error   { return r.l.Error(eid, msg) }
func (r realEventLog) Close() error                         { return r.l.Close() }

// NewEventLogHandler opens the Event Log source named source and returns a
// handler that mirrors cataloged events into it, plus a Closer for the handle.
//
// It must be called before the configuration is resolved. A failure to start is
// the case this sink exists for, and a config error happens before any log file
// could have been opened — so the Event Log arm has to be installed first or it
// cannot report the one thing it is here for.
//
// The source must already be registered (InstallEventLogSource, which the
// installer calls). Opening an unregistered source succeeds but renders every
// entry as "The description for Event ID cannot be found", so the error here is
// the honest place to notice.
func NewEventLogHandler(source string) (slog.Handler, io.Closer, error) {
	l, err := eventlog.Open(source)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the event log source %q (is the service installed?): %w", source, err)
	}

	w := realEventLog{l: l}

	return &eventLogHandler{w: w}, w, nil
}

// InstallEventLogSource registers source so its entries render as text.
//
// Without the registration the writes succeed and Event Viewer shows "The
// description for Event ID ... cannot be found" for every one of them, which is
// the worst of both worlds: the operator sees that something happened and
// cannot read what.
//
// Registering an existing source is not an error here. eventlog.Install refuses
// it, and a reinstall over a live service is the ordinary case rather than a
// mistake.
func InstallEventLogSource(source string) error {
	err := eventlog.InstallAsEventCreate(source, eventlog.Info|eventlog.Warning|eventlog.Error)
	if err == nil {
		return nil
	}

	// Ask the registry rather than match x/sys's error text. The string is
	// exact today, but it is an unexported implementation detail of a
	// dependency we do not control, and the failure mode if it changes is an
	// install that refuses to re-run over its own previous one.
	if sourceRegistered(source) {
		return nil
	}

	return fmt.Errorf("registering the event log source %q (elevated?): %w", source, err)
}

// sourceRegistered reports whether the event log source key already exists.
func sourceRegistered(source string) bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, eventLogKey+`\`+source, registry.QUERY_VALUE)
	if err != nil {
		return false
	}

	_ = k.Close()

	return true
}

// RemoveEventLogSource deregisters source. A source that is already gone is not
// an error, so an interrupted uninstall can be re-run.
func RemoveEventLogSource(source string) error {
	if err := eventlog.Remove(source); err != nil {
		if errors.Is(err, errSourceNotFound) {
			return nil
		}

		return fmt.Errorf("removing the event log source %q: %w", source, err)
	}

	return nil
}
