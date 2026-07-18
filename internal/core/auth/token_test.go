/*
Testing: token.go

Pending:

Tested:
  Generate
    - TestGenerate_ShouldProduceUniquePrefixedTokens: prefixed, high-entropy, never repeats.
  Hash
    - TestHash_ShouldBeStableAndAlgorithmTagged: same input same output, "sha256:" tagged.
    - TestHash_ShouldDifferPerToken: distinct tokens never share a hash.
  Mask
    - TestMask_ShouldRevealOnlyTheTail: only the last 4 chars survive.
    - TestMask_ShouldNotRevealShortInput: a short value is fully masked.

Tested elsewhere:

Declined:

Additional Remarks:
  Generate's randomness is not asserted statistically — that would test
  crypto/rand, not this code. Uniqueness across a batch plus the decoded length
  is what this package is responsible for.
*/

package auth

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerate_ShouldProduceUniquePrefixedTokens(t *testing.T) {
	t.Parallel()

	// ARRANGE
	const batch = 100

	seen := make(map[string]bool, batch)

	// ACT
	for range batch {
		token, err := Generate()
		require.NoError(t, err)

		// ASSERT — prefixed, full-entropy, and never seen before.
		require.True(t, strings.HasPrefix(token, TokenPrefix), "token %q should carry the prefix", token)

		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, TokenPrefix))
		require.NoError(t, err)
		assert.Len(t, raw, tokenBytes)

		require.False(t, seen[token], "Generate returned a duplicate token")
		seen[token] = true
	}
}

func TestHash_ShouldBeStableAndAlgorithmTagged(t *testing.T) {
	t.Parallel()

	// ARRANGE
	const sample = "wadapt_example"

	// ACT
	first, second := Hash(sample), Hash(sample)

	// ASSERT — stable, self-describing, and not the token itself.
	assert.Equal(t, first, second)
	assert.True(t, strings.HasPrefix(first, hashAlgorithm+":"), "hash %q should be algorithm-tagged", first)
	assert.NotContains(t, first, sample)
}

func TestHash_ShouldDifferPerToken(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT
	first, second := Hash("wadapt_one"), Hash("wadapt_two")

	// ASSERT
	assert.NotEqual(t, first, second)
}

func TestMask_ShouldRevealOnlyTheTail(t *testing.T) {
	t.Parallel()

	// ARRANGE
	const sample = "wadapt_supersecretvaluek123"

	// ACT
	masked := Mask(sample)

	// ASSERT — the tail identifies it; nothing else survives.
	assert.Equal(t, "****k123", masked)
	assert.NotContains(t, masked, "supersecret")
}

func TestMask_ShouldNotRevealShortInput(t *testing.T) {
	t.Parallel()

	// ARRANGE / ACT — a value too short to keep a tail from must not leak it.
	masked := Mask("abc")

	// ASSERT
	assert.Equal(t, "***", masked)
}
