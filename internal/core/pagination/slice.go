package pagination

import (
	"cmp"
	"net/url"
	"slices"
)

// Slice cuts one page out of an already-sorted collection and mints the cursor
// that resumes after it.
//
// This is the keyset-pagination sequence every list endpoint needs, and it is
// here because it was written twice — once for the DHCP scopes collection and
// once for the demo resource — line for line, down to the step-over-found-key
// branch. Two consumers is the threshold this repo hoists at, and the precedent
// is not a style preference: jsonshape was hoisted at two consumers and the
// second copy turned out to have dropped a load-bearing branch. M3b's leases and
// reservations would have made copies three and four of logic the pagination
// design doc itself calls safety-critical.
//
// # Contract
//
// items MUST already be sorted ascending by key(item), and key MUST be unique
// across them. Both are the caller's to guarantee, and neither is checked here —
// a linear scan to verify would cost more than the page it is serving, on every
// request.
//
// Neither is a formality. The cursor is a resume key rather than an offset, so
// the sort and the resume must compare the same thing: sorting on one form and
// resuming on another skips and repeats pages in silence. (IPv4 dotted strings
// are the standing trap — "192.168.178.0" sorts before "192.168.2.0" as text and
// after it as an address.) A repeated key is worse: BinarySearchFunc finds one
// of the duplicates, the step-over skips exactly one, and the rest of the run is
// dropped from the walk with nothing to show it happened.
//
// # Behaviour
//
// An item deleted between pages is simply not found, and the walk resumes at the
// next one rather than restarting or skipping — the property a resume key has
// and an offset does not.
//
// The returned NextPage is zero when no items remain, and a zero NextPage
// renders as the last page. Presence of the cursor, not a full page, is what
// tells a client to ask again; a short page is not a stop signal.
func Slice[T any](
	p Paginator, items []T, params Params, requestURL *url.URL, key func(T) string,
) ([]T, NextPage) {
	start := 0

	if params.After != "" {
		// BinarySearchFunc lands on the first item ordered at or after the cursor
		// key; when that key is still present, step over it so the page begins
		// after the previous page's last item rather than repeating it.
		found := false

		start, found = slices.BinarySearchFunc(items, params.After, func(item T, after string) int {
			return cmp.Compare(key(item), after)
		})
		if found {
			start++
		}
	}

	end := min(start+params.Size, len(items))
	page := items[start:end]

	// A cursor only when items remain. On the last page both forms are absent
	// together, which is what tells a client to stop.
	var next NextPage
	if end < len(items) {
		next = p.Next(requestURL, key(page[len(page)-1]))
	}

	return page, next
}
