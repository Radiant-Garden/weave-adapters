//go:build !windows

/*
Testing: service_other.go

Pending:

Tested:

	NewManager -> - TestNewManager_ShouldRefuseOffWindows

Tested elsewhere:

	Everything the Manager interface exists for: the service subcommand's own
	tests in cmd/weave-adapter-dhcp-windows/service_test.go, which drive a fake
	Manager and therefore run here.

Declined:

	Nothing.

Additional Remarks:

	This stub is one line, and it is one line because Manager is an interface.
	Had NewManager returned a concrete type, every refusal in the service
	subcommand would only be checkable on a Windows host.
*/
package winsvc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewManager_ShouldRefuseOffWindows(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	m, err := NewManager()

	// ASSERT
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Nil(t, m)
}
