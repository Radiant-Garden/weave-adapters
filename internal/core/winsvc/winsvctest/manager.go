// Package winsvctest provides test doubles for the injectable seams in
// internal/core/winsvc.
//
// It exists so that every caller of those seams tests against ONE fake rather
// than a hand-written copy per package. Two fakes kept in step by hand are
// exactly the drift winsvc's injection exists to prevent: the seam was
// introduced so the decisions around an SCM call — the consent gate, the
// refusals, which paths get secured, what gets registered — could be tested on
// any platform, and a second fake that answers slightly differently makes one
// of those test suites quietly stop meaning anything.
//
// internal/core/events/testing is the precedent for a testing helper living
// beside the package it doubles rather than inside a _test.go file.
//
// These doubles RECORD; they do not simulate. What a test asserts with them is
// what the caller ASKED the SCM to do. Whether the SCM honours it is a
// different question, answered by task service-gate against a live one.
package winsvctest

import (
	"github.com/radiantgarden/weave-adapters/internal/core/winsvc"
)

// Compile-time conformance. Without it a method whose signature drifts from the
// interface fails at each call site instead of here, which reads as a broken
// test rather than a stale double.
var _ winsvc.Manager = (*Manager)(nil)

// Manager is a winsvc.Manager that records what it was asked to do.
//
// The zero value is usable: it accepts every operation, reports a service that
// is not installed, and records the calls.
type Manager struct {
	// Installed holds every Definition Install accepted, in order.
	Installed []winsvc.Definition

	// Calls names each operation in the order it was made, so a test can
	// assert both what happened and what did not — a refusal that still
	// reached the SCM has already done the thing it was refusing.
	Calls []string

	// Reported is what Status answers. Named for what it does rather than
	// after the method, which cannot share the name.
	Reported winsvc.ServiceStatus

	// InstallErr, when set, is returned by Install instead of recording the
	// definition — for the already-installed path above all.
	InstallErr error

	// OpErr, when set, is returned by Uninstall, Start and Stop.
	OpErr error

	// StatusErr, when set, is returned by Status.
	StatusErr error

	// Closed records whether the connection was released. A command that opens
	// an SCM handle and leaks it is a bug no assertion on output would catch.
	Closed bool
}

// Install records the definition, or fails with InstallErr.
func (m *Manager) Install(d winsvc.Definition) error {
	m.Calls = append(m.Calls, "install")

	if m.InstallErr != nil {
		return m.InstallErr
	}

	m.Installed = append(m.Installed, d)

	return nil
}

// Uninstall records the call and returns OpErr.
func (m *Manager) Uninstall(string) error {
	m.Calls = append(m.Calls, "uninstall")

	return m.OpErr
}

// Start records the call and returns OpErr.
func (m *Manager) Start(string) error {
	m.Calls = append(m.Calls, "start")

	return m.OpErr
}

// Stop records the call and returns OpErr.
func (m *Manager) Stop(string) error {
	m.Calls = append(m.Calls, "stop")

	return m.OpErr
}

// Status records the call and answers Reported.
func (m *Manager) Status(string) (winsvc.ServiceStatus, error) {
	m.Calls = append(m.Calls, "status")

	if m.StatusErr != nil {
		return winsvc.ServiceStatus{}, m.StatusErr
	}

	return m.Reported, nil
}

// Close marks the connection released.
func (m *Manager) Close() error {
	m.Closed = true

	return nil
}

// Securer is a stand-in for winsvc.Secure that records what the lockdown was
// asked to cover.
type Securer struct {
	// Targets holds every Securable passed to Secure, in order.
	Targets []winsvc.Securable

	// Err, when set, fails the whole call.
	Err error

	// Skip reports every target as not applied, which is what a target that
	// does not exist yet looks like — the token store legitimately does not,
	// before the first mint. A test asserting the skipped-target message has
	// to ask for this.
	Skip bool
}

// Secure records the targets and reports a result for each.
func (s *Securer) Secure(targets []winsvc.Securable) ([]winsvc.SecureResult, error) {
	s.Targets = append(s.Targets, targets...)

	if s.Err != nil {
		return nil, s.Err
	}

	results := make([]winsvc.SecureResult, 0, len(targets))
	for _, t := range targets {
		results = append(results, winsvc.SecureResult{Target: t, Applied: !s.Skip})
	}

	return results, nil
}
