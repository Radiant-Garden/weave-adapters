/*
Testing: slice.go

Pending:

Tested:
  Slice
    - TestSlice_ShouldReturnTheFirstPageWhenNoCursorIsGiven: an absent After starts at the beginning.
    - TestSlice_ShouldResumeAfterTheCursorKey: the page begins after the previous page's last key, never repeating it.
    - TestSlice_ShouldMintNoCursorOnTheLastPage: a page that exhausts the collection carries a zero NextPage.
    - TestSlice_ShouldMintACursorWhileItemsRemain: a full page short of the end carries the resume cursor.
    - TestSlice_ShouldResumeAfterADeletedCursorKey: a key no longer present resumes at the next item rather than skipping or restarting.
    - TestSlice_ShouldRoundTripThroughItsOwnCursor: page one's minted After walks to page two with no gap and no repeat.

Tested elsewhere:
  The two production consumers exercise Slice end to end through the real
  middleware chain: the DHCP scopes collection in the adapter's scopes_test.go,
  and the demo resource in httptest/demo_test.go (list -> ETag -> 304 -> page 2).

Declined:
  The two preconditions — items sorted by key, keys unique — are the caller's to
  guarantee and are documented as unchecked, so there is nothing here to assert
  about violating them: Slice's behaviour on unsorted or duplicate input is
  explicitly undefined, and pinning a particular wrong answer would turn a
  documented non-contract into an accidental one.

Additional Remarks:
  The fixtures use single-character string keys so their sort order and their
  cursor round-trip are both readable at a glance; the production keys are
  wadaptIDs and demo IDs, covered where those types live.
*/

package pagination

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slicePaginator returns a paginator sized so a test can pick page boundaries by
// choosing Size, and a request URL the minted cursor is built against.
func slicePaginator() (Paginator, *url.URL) {
	reqURL, _ := url.Parse("/api/v1/things?pageSize=2")

	return New("slice-test", 2, 100), reqURL
}

// letters is a sorted, unique key set — Slice's two preconditions, met.
func letters() []string {
	return []string{"a", "b", "c", "d", "e"}
}

// key is the identity projection for a string item.
func key(s string) string { return s }

func TestSlice_ShouldReturnTheFirstPageWhenNoCursorIsGiven(t *testing.T) {
	t.Parallel()

	// ARRANGE
	pages, reqURL := slicePaginator()

	// ACT — no After, so the page starts at the beginning.
	page, next := Slice(pages, letters(), Params{Size: 2}, reqURL, key)

	// ASSERT
	assert.Equal(t, []string{"a", "b"}, page)
	assert.NotEmpty(t, next.Token, "a full page short of the end must carry a cursor")
}

func TestSlice_ShouldResumeAfterTheCursorKey(t *testing.T) {
	t.Parallel()

	// ARRANGE — resume after "b", which is present.
	pages, reqURL := slicePaginator()

	// ACT
	page, _ := Slice(pages, letters(), Params{Size: 2, After: "b"}, reqURL, key)

	// ASSERT — the page begins after b, not at it.
	assert.Equal(t, []string{"c", "d"}, page)
}

func TestSlice_ShouldMintNoCursorOnTheLastPage(t *testing.T) {
	t.Parallel()

	// ARRANGE — a page that reaches the final item.
	pages, reqURL := slicePaginator()

	// ACT
	page, next := Slice(pages, letters(), Params{Size: 2, After: "c"}, reqURL, key)

	// ASSERT — d, e, and then nothing left; both cursor forms absent.
	assert.Equal(t, []string{"d", "e"}, page)
	assert.Empty(t, next.Token, "the last page must not carry a cursor")
	assert.Empty(t, next.URL)
}

func TestSlice_ShouldMintACursorWhileItemsRemain(t *testing.T) {
	t.Parallel()

	// ARRANGE
	pages, reqURL := slicePaginator()

	// ACT — one item short of the end.
	page, next := Slice(pages, letters(), Params{Size: 2, After: "a"}, reqURL, key)

	// ASSERT
	assert.Equal(t, []string{"b", "c"}, page)
	require.NotEmpty(t, next.Token)

	// The cursor resumes after the page's last key.
	after, ok := pages.parseToken(next.Token)
	require.True(t, ok)
	assert.Equal(t, "c", after)
}

func TestSlice_ShouldResumeAfterADeletedCursorKey(t *testing.T) {
	t.Parallel()

	// ARRANGE — the client holds a cursor for "b", but b was deleted between
	// pages, so the collection no longer contains it.
	pages, reqURL := slicePaginator()
	without := []string{"a", "c", "d", "e"}

	// ACT
	page, _ := Slice(pages, without, Params{Size: 2, After: "b"}, reqURL, key)

	// ASSERT — the walk resumes at the next key after b rather than restarting at
	// a or skipping past c. This is the property a resume key has and an offset
	// does not.
	assert.Equal(t, []string{"c", "d"}, page)
}

func TestSlice_ShouldRoundTripThroughItsOwnCursor(t *testing.T) {
	t.Parallel()

	// ARRANGE
	pages, reqURL := slicePaginator()
	items := letters()

	// ACT — page one, then feed its cursor back for page two.
	page1, next1 := Slice(pages, items, Params{Size: 2}, reqURL, key)
	require.NotEmpty(t, next1.Token)

	after, ok := pages.parseToken(next1.Token)
	require.True(t, ok)

	page2, _ := Slice(pages, items, Params{Size: 2, After: after}, reqURL, key)

	// ASSERT — the two pages are consecutive: no key repeated, none skipped.
	assert.Equal(t, []string{"a", "b"}, page1)
	assert.Equal(t, []string{"c", "d"}, page2)
}
