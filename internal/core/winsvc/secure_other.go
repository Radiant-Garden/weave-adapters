//go:build !windows

package winsvc

// Secure always fails off Windows. The policy it applies is portable data and
// is tested here; only the application of it needs the platform.
func Secure([]Securable) ([]SecureResult, error) { return nil, ErrUnsupported }

// ReadSecurity always fails off Windows.
func ReadSecurity(string) (Security, error) { return Security{}, ErrUnsupported }
