//go:build !windows

package winsvc

// NewManager always fails off Windows.
//
// The service subcommand's own logic — flag parsing, the consent gate, the
// refusals — is tested against a fake Manager rather than this, which is the
// whole reason Manager is an interface.
func NewManager() (Manager, error) { return nil, ErrUnsupported }
