/*
Testing: mint.go

Pending:

Tested:
  ExpiryInDays
    - TestExpiryInDays_ShouldLandTheRequestedNumberOfDaysLater: the arithmetic, in UTC whatever the caller passed.
    - TestExpiryInDays_ShouldRefuseAnExpiryTheStoreCouldNotWriteBack: the round-trip check, not an invented ceiling.
    - TestExpiryInDays_ShouldRequireAPositiveNumberOfDays: "never expires" is a nil Expiry, not a zero one.
  Store.Mint
    - TestMint_ShouldReturnTheTokenAndStoreOnlyItsHash: the token reaches the caller and never the store.
    - TestMint_ShouldRecordTheExpiryItWasGiven: a nil expiry means never, a real one is carried onto the entry.
    - TestMint_ShouldRejectADuplicateLabelWithoutAddingAnything: Add-before-Save, from the Add side.
    - TestMint_ShouldRejectAnInvalidLabel: the label charset is Add's, and Mint does not route around it.

Tested elsewhere:
  Save, and that Mint's entry survives a round-trip to disk: store_test.go.

  The CLI layered on top — the flag that spells the day count, the message a
  too-large one produces, and the printing of the token exactly once:
  cmd/weave-adapter-dhcp-windows/token_test.go.

Declined:
  Asserting the token's entropy or format. Generate owns both and token_test.go
  covers them; re-checking here would test the same function twice.

Additional Remarks:
  Both functions moved out of the token CLI in M4b Phase 0, so that setup could
  mint without reimplementing the sequence. Two parts of it are easy to drop in
  an extract and have no obvious symptom when dropped, which is why each has a
  test of its own here rather than only through the CLI:

  Add runs before Save. Mint does the Add and leaves the Save to the caller, so
  a duplicate label fails with the file untouched. A Mint that saved itself
  would still be correct today and would make the ordering invisible.

  The expiry is bounded by what the store can write back rather than by a
  number chosen here. A ceiling would drift from MarshalText the first time
  either changed, and the failure it exists to prevent — a token file every
  later Load rejects — is only recoverable by hand-editing.
*/

package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpiryInDays_ShouldLandTheRequestedNumberOfDaysLater(t *testing.T) {
	t.Parallel()

	// ARRANGE — a non-UTC zone, so the normalization is actually exercised.
	zone := time.FixedZone("test-plus-5", 5*60*60)
	from := time.Date(2026, time.September, 18, 12, 0, 0, 0, zone)

	// ACT
	expiry, err := ExpiryInDays(from, 90)

	// ASSERT
	require.NoError(t, err)
	require.NotNil(t, expiry)

	assert.Equal(t, from.UTC().AddDate(0, 0, 90), expiry.Time())

	// Stored in UTC whatever zone the shell that ran the command was in,
	// otherwise the same instant reads differently in two token files.
	assert.Equal(t, time.UTC, expiry.Time().Location())
}

func TestExpiryInDays_ShouldRefuseAnExpiryTheStoreCouldNotWriteBack(t *testing.T) {
	t.Parallel()

	// ARRANGE — far enough out to push the year past RFC 3339's four digits.
	from := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)

	// ACT
	expiry, err := ExpiryInDays(from, 9_999_999)

	// ASSERT
	// The bound is MarshalText's, not a number chosen here: a ceiling would
	// drift from what the store enforces, and the file it would have written
	// is one every later Load rejects.
	require.Error(t, err)
	assert.Nil(t, expiry)
	assert.Contains(t, err.Error(), "cannot be stored")
	assert.Contains(t, err.Error(), "four-digit range")
}

func TestExpiryInDays_ShouldRequireAPositiveNumberOfDays(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)

	tests := map[string]int{
		"should reject zero days":     0,
		"should reject negative days": -1,
	}

	for name, days := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ACT
			expiry, err := ExpiryInDays(from, days)

			// ASSERT
			// Never-expires is the absence of an Expiry. Reading a zero here as
			// "never" would put one caller's flag convention in this package
			// and answer (nil, nil), which no caller could tell from a bug.
			require.Error(t, err)
			assert.Nil(t, expiry)
			assert.Contains(t, err.Error(), "positive number of days")
		})
	}
}

func TestMint_ShouldReturnTheTokenAndStoreOnlyItsHash(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := &Store{}
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)

	// ACT
	token, err := store.Mint("weave-prod", now, nil)

	// ASSERT
	require.NoError(t, err)
	require.NotEmpty(t, token)
	assert.Contains(t, token, TokenPrefix)

	require.Len(t, store.Tokens, 1)

	entry := store.Tokens[0]
	assert.Equal(t, "weave-prod", entry.Label)
	assert.Equal(t, now, entry.CreatedAt)

	// The whole reason this file is not a credential.
	assert.Equal(t, Hash(token), entry.Hash)
	assert.NotContains(t, entry.Hash, token)
}

func TestMint_ShouldRecordTheExpiryItWasGiven(t *testing.T) {
	t.Parallel()

	// ARRANGE
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)

	expiry, err := ExpiryInDays(now, 30)
	require.NoError(t, err)

	tests := map[string]struct {
		expiresAt *Expiry
		wantNil   bool
	}{
		"should leave the entry unexpiring when given no expiry": {expiresAt: nil, wantNil: true},
		"should carry the expiry onto the entry":                 {expiresAt: expiry},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// ARRANGE
			store := &Store{}

			// ACT
			_, err := store.Mint("weave-prod", now, tc.expiresAt)

			// ASSERT
			require.NoError(t, err)
			require.Len(t, store.Tokens, 1)

			if tc.wantNil {
				assert.Nil(t, store.Tokens[0].ExpiresAt)

				return
			}

			require.NotNil(t, store.Tokens[0].ExpiresAt)
			assert.Equal(t, expiry.Time(), store.Tokens[0].ExpiresAt.Time())
		})
	}
}

func TestMint_ShouldRejectADuplicateLabelWithoutAddingAnything(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := &Store{}
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)

	first, err := store.Mint("weave-prod", now, nil)
	require.NoError(t, err)

	// ACT
	second, err := store.Mint("weave-prod", now, nil)

	// ASSERT
	// This is the half of Add-before-Save that lives in this package: the
	// caller Saves only on success, so a refused label never reaches the file.
	require.ErrorIs(t, err, ErrDuplicateLabel)
	assert.Empty(t, second)

	require.Len(t, store.Tokens, 1)
	assert.Equal(t, Hash(first), store.Tokens[0].Hash, "the first token's entry was replaced")
}

func TestMint_ShouldRejectAnInvalidLabel(t *testing.T) {
	t.Parallel()

	// ARRANGE
	store := &Store{}
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)

	// ACT — a label that would not be safe as a log field or a TOML key.
	token, err := store.Mint("weave prod!", now, nil)

	// ASSERT
	// Minting is not a way around Add's charset: the label becomes the caller
	// subject on every event the token's requests emit.
	require.ErrorIs(t, err, ErrInvalidLabel)
	assert.Empty(t, token)
	assert.Empty(t, store.Tokens)
}
