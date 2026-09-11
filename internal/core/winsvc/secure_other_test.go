//go:build !windows

/*
Testing: secure_other.go

Pending:

Tested:

	Secure / ReadGrants -> - TestSecureStubs_ShouldRefuseOffWindows

Tested elsewhere:

	The policy these stand in for: secure_test.go, which is portable.

Declined:

	Nothing.

Additional Remarks:

	The split is the same one the controller uses, and for the same reason:
	the decisions -- who is granted what, what inherits, what counts as a
	write -- are portable data tested on every host, and only the syscall that
	applies them needs the platform.
*/
package winsvc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecureStubs_ShouldRefuseOffWindows(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	results, err := Secure(SecurablesFor(`C:\cfg\c.toml`, "", ""))
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Nil(t, results)

	grants, err := ReadGrants(`C:\cfg\c.toml`)
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Nil(t, grants)
}
