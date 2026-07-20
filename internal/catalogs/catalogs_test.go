/*
Testing: catalogs.go

Pending:

Tested:

	the registration list itself
	  - TestCatalogs_ShouldRegisterEveryCategoryTheRepoDefines: importing this
	    package is enough to see core's events and every adapter's, which is the
	    one thing it exists to guarantee.
	  - TestCatalogs_ShouldRegisterEveryAdapterEventsPackage: the list is compared
	    against the packages that exist on disk, so a new adapter that forgets its
	    line fails here by name instead of failing somewhere unrelated.

Tested elsewhere:

	What the events themselves say: each catalog's own tests, plus the registry's
	contract checks, which panic at init on a malformed entry — so anything
	importing this package has already validated every event in it.

	That docs/events.md matches the registry: generate-check.

	That errors.yaml's ProblemType enum lists exactly the live response codes:
	api/common/common_test.go, which is the other consumer of this package.

Declined:

	Asserting an exact event count, or an exact set of IDs. Both would fail on
	every added event, which is a change this package has no opinion about — the
	invariant is that each catalog is *linked*, not what it contains.

Additional Remarks:

	TestCatalogs_ShouldRegisterEveryAdapterEventsPackage reads the adapters
	directory rather than hardcoding a list, because a hardcoded list is the same
	thing the blank imports already are — a second copy to forget. Discovering
	the packages is what makes the test fail for the adapter nobody registered.
*/
package catalogs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
)

// adaptersDir is where an adapter's events package lives, relative to here.
const adaptersDir = "../adapters"

func TestCatalogs_ShouldRegisterEveryCategoryTheRepoDefines(t *testing.T) {
	t.Parallel()

	// ARRANGE — importing this package is the entire arrangement; the blank
	// imports have run their init()s by now.
	registered := events.GetAll()
	require.NotEmpty(t, registered, "importing catalogs must populate the registry")

	// ACT
	categories := make(map[string]bool)
	for _, spec := range registered {
		categories[spec.Category] = true
	}

	// ASSERT — core's categories and the adapter's, in one registry. A missing
	// blank import shows up here rather than as a confusing failure about
	// errors.yaml declaring a ghost enum entry, which is how it surfaced the
	// first time.
	for _, category := range []string{"SYS", "API", "HLT", "BACKEND", "DHCP"} {
		assert.True(t, categories[category],
			"no %s events registered — is that catalog missing from catalogs.go?", category)
	}
}

func TestCatalogs_ShouldRegisterEveryAdapterEventsPackage(t *testing.T) {
	t.Parallel()

	// ARRANGE — the adapters that exist on disk, discovered rather than listed.
	// A hardcoded list here would be a second copy of the blank imports, and
	// therefore a second thing to forget.
	entries, err := os.ReadDir(adaptersDir)
	require.NoError(t, err)

	var expected []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// An adapter registers events only if it has an events package.
		if _, statErr := os.Stat(filepath.Join(adaptersDir, entry.Name(), "events")); statErr != nil {
			continue
		}

		expected = append(expected, entry.Name())
	}

	require.NotEmpty(t, expected, "no adapter events packages found — has the layout changed?")

	// ACT — the import block, read as text. The alternative is asserting on
	// registered IDs, which cannot distinguish "this adapter is linked" from
	// "some other adapter happens to use the same category".
	source, err := os.ReadFile("catalogs.go")
	require.NoError(t, err)

	// ASSERT — every adapter with an events package is imported here. This is
	// the check that names the culprit: without it, forgetting the line fails
	// somewhere else entirely, or silently passes.
	for _, adapter := range expected {
		assert.Contains(t, string(source),
			"internal/adapters/"+adapter+"/events",
			"adapter %q has an events package that catalogs.go does not import; "+
				"its events would be missing from docs/events.md while generate-check still passed, "+
				"and its response codes would read as ghost entries in errors.yaml", adapter)
	}

	// Core's own catalog too, which is not under adapters/ and so is not
	// discovered above.
	assert.Contains(t, string(source), "internal/core/events/catalog")

	// The import block is blank imports only. A non-blank one would make this a
	// package with behaviour, and something would eventually depend on it for
	// more than registration.
	for line := range strings.SplitSeq(string(source), "\n") {
		if strings.Contains(line, "weave-adapters/internal/") {
			assert.True(t, strings.HasPrefix(strings.TrimSpace(line), "_ \""),
				"imports here exist only for their init(): %s", strings.TrimSpace(line))
		}
	}
}
