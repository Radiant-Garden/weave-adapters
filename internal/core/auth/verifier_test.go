/*
Testing: verifier.go

Pending:

Tested:
  NewVerifier / Len
    - TestLen_ShouldCountConfiguredTokens: counts every entry, expired ones included.
  Usable
    - TestUsable_ShouldCountOnlyTokensThatCanStillAuthenticate: startup uses it to refuse a store that accepts nothing.
  Verify
    - TestVerify_ShouldResolveAConfiguredToken: the happy path returns the entry.
    - TestVerify_ShouldRejectUnknownToken: an unconfigured token resolves to ErrUnknownToken.
    - TestVerify_ShouldRejectExpiredToken: a matched but expired token is distinguished for the operator.
    - TestVerify_ShouldMatchAnyOfSeveralTokens: rotation keeps both tokens live.
    - TestVerify_ShouldNotMatchOnHashPrefix: a truncated or prefix hash never authenticates.

Tested elsewhere:
  The response an operator or attacker sees for each outcome is covered in
  bearer_test.go; Verify only classifies.

Declined:

Additional Remarks:
  Expiry uses an injected clock so the boundary case is deterministic.

  Verify's constant-time property is not asserted — timing assertions are
  famously flaky under a shared CI runner, and the guarantee comes from
  subtle.ConstantTimeCompare over fixed-length hashes, which is visible in the
  code. What is asserted is the part that could silently regress: that matching
  happens on the full hash, never a prefix.
*/

package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newVerifier builds a Verifier pinned to testClock.
func newVerifier(entries ...Entry) *Verifier {
	v := NewVerifier(entries)
	v.now = func() time.Time { return testClock }

	return v
}

// entryFor returns an entry whose hash matches the given token.
func entryFor(label, token string, expires *Expiry) Entry {
	return Entry{Label: label, Hash: Hash(token), CreatedAt: testClock, ExpiresAt: expires}
}

func TestLen_ShouldCountConfiguredTokens(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT / ASSERT
	assert.Equal(t, 0, newVerifier().Len())
	assert.Equal(t, 2, newVerifier(entryFor("a", "t1", nil), entryFor("b", "t2", nil)).Len())
}

func TestUsable_ShouldCountOnlyTokensThatCanStillAuthenticate(t *testing.T) {
	t.Parallel()

	// ARRANGE — the store that motivates the method: entries are present, and
	// none of them can match anything.
	expired := NewExpiry(testClock.Add(-time.Hour))
	live := NewExpiry(testClock.Add(time.Hour))

	// ACT / ASSERT
	allExpired := newVerifier(entryFor("a", "t1", expired), entryFor("b", "t2", expired))
	assert.Equal(t, 2, allExpired.Len(), "the entries are there")
	assert.Equal(t, 0, allExpired.Usable(), "and not one of them can authenticate")

	mixed := newVerifier(entryFor("a", "t1", expired), entryFor("b", "t2", live), entryFor("c", "t3", nil))
	assert.Equal(t, 2, mixed.Usable(), "a never-expiring token counts, an expired one does not")
}

func TestVerify_ShouldResolveAConfiguredToken(t *testing.T) {
	t.Parallel()

	// ARRANGE
	v := newVerifier(entryFor("weave-prod", "wadapt_good", nil))

	// ACT
	entry, err := v.Verify("wadapt_good")

	// ASSERT
	require.NoError(t, err)
	assert.Equal(t, "weave-prod", entry.Label)
}

func TestVerify_ShouldRejectUnknownToken(t *testing.T) {
	t.Parallel()

	// ARRANGE
	v := newVerifier(entryFor("weave-prod", "wadapt_good", nil))

	// ACT
	_, err := v.Verify("wadapt_wrong")

	// ASSERT
	require.ErrorIs(t, err, ErrUnknownToken)
}

func TestVerify_ShouldRejectExpiredToken(t *testing.T) {
	t.Parallel()

	// ARRANGE — configured, matching, but past its expiry.
	expired := NewExpiry(testClock.Add(-time.Hour))
	v := newVerifier(entryFor("weave-prod", "wadapt_good", expired))

	// ACT
	entry, err := v.Verify("wadapt_good")

	// ASSERT — the label comes back so the operator event can name which token
	// expired, even though the caller is told nothing.
	require.ErrorIs(t, err, ErrTokenExpired)
	assert.Equal(t, "weave-prod", entry.Label)
}

func TestVerify_ShouldMatchAnyOfSeveralTokens(t *testing.T) {
	t.Parallel()

	// ARRANGE — mid-rotation: the old and new tokens are both live.
	v := newVerifier(
		entryFor("weave-old", "wadapt_old", nil),
		entryFor("weave-new", "wadapt_new", nil),
	)

	// ACT / ASSERT — this is what makes a zero-downtime rotation possible.
	old, err := v.Verify("wadapt_old")
	require.NoError(t, err)
	assert.Equal(t, "weave-old", old.Label)

	fresh, err := v.Verify("wadapt_new")
	require.NoError(t, err)
	assert.Equal(t, "weave-new", fresh.Label)
}

func TestVerify_ShouldNotMatchOnHashPrefix(t *testing.T) {
	t.Parallel()

	// ARRANGE — an entry whose stored hash is a truncated prefix of the real one.
	full := Hash("wadapt_good")

	v := newVerifier(Entry{Label: "truncated", Hash: full[:20], CreatedAt: testClock})

	// ACT
	_, err := v.Verify("wadapt_good")

	// ASSERT — comparing on anything but the whole hash would let a shortened
	// entry authenticate a token it does not equal.
	require.ErrorIs(t, err, ErrUnknownToken)
}
