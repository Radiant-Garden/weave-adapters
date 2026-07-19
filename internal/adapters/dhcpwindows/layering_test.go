/*
Testing: the core/adapter layering rule (no corresponding .go file)

Pending:

Tested:

  - TestLayering_ShouldKeepCoreFreeOfAdapterImports: no package under
    internal/core imports internal/adapters. This is the CLAUDE.md rule that
    core stays adapter-agnostic, and dhcpwindows is the first package that
    could ever break it.
  - TestLayering_ShouldKeepTheAdapterOffTheCoreCatalog: the adapter does not
    register events in the core catalog package.

Tested elsewhere:

	The same technique applied to the test-only demo resource:
	  internal/core/httptest/demo_test.go's TestDemo_ShouldNotBeReachableFromTheBinary,
	  which is the precedent this file follows.

Declined:

	Asserting the reverse direction (that the adapter *may* import core): it does,
	  visibly, in config.go — a test would restate an import statement.
	Enforcing that the binary links the adapter: it does not yet, and will from
	  Phase 1. Asserting it now would fail for a correct tree.

Additional Remarks:

	This file has no .go counterpart because the invariant is about the module
	  graph rather than about any one package's behaviour. It lives here, in the
	  first package under internal/adapters/, because that is the package whose
	  existence makes the rule violable at all — before it, the rule was
	  vacuously true.

	It shells out to `go list`, so it needs the toolchain and the module source at
	  run time, exactly like the demo precedent. It cannot pass from a prebuilt
	  `go test -c` binary on a host without them.
*/
package dhcpwindows_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const modulePath = "github.com/radiantgarden/weave-adapters"

// deps returns every package the given pattern transitively depends on.
func deps(t *testing.T, pattern string) []string {
	t.Helper()

	//nolint:gosec // G204: pattern is a compile-time constant from this file.
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", pattern).CombinedOutput()
	require.NoError(t, err, "go list failed: %s", out)

	list := strings.Split(strings.TrimSpace(string(out)), "\n")
	require.Greater(t, len(list), 10, "go list should report a real dependency set, got %q", out)

	return list
}

func TestLayering_ShouldKeepCoreFreeOfAdapterImports(t *testing.T) {
	t.Parallel()

	// ARRANGE — everything every core package transitively pulls in.
	all := deps(t, modulePath+"/internal/core/...")

	// ACT
	var leaked []string

	for _, pkg := range all {
		if strings.HasPrefix(pkg, modulePath+"/internal/adapters") {
			leaked = append(leaked, pkg)
		}
	}

	// ASSERT — core is adapter-agnostic: if a core package would stop compiling
	// once the backend service is ripped out, it belongs in an adapter package.
	// A decision no build enforces is a comment, and this is the first moment
	// the rule could be broken at all — before internal/adapters/ existed it was
	// vacuously true.
	//
	// The pressure is real and arrives next phase: httpserver has to serve the
	// adapter's OpenAPI spec and mount its routes. Both must arrive as values
	// passed inward, never as an import.
	assert.Empty(t, leaked, "internal/core must not import internal/adapters")
}

func TestLayering_ShouldKeepTheAdapterOffTheCoreCatalog(t *testing.T) {
	t.Parallel()

	// ARRANGE
	all := deps(t, modulePath+"/internal/adapters/dhcpwindows")

	// ASSERT — the adapter registers its own events in its own package. The
	// BACKEND *category* constant is shared and lives in core, but each adapter
	// owns a partitioned ID range (BACKEND-1xx here), because the single-owner
	// rule breaks the moment a second adapter wants to emit a backend failure
	// from an ID registered in the core catalog.
	//
	// Nothing is registered yet: the client returns typed errors and no emitter
	// exists until the probe lands, and registering an event nothing emits
	// would be a ghost event.
	assert.NotContains(t, all, modulePath+"/internal/core/events/catalog",
		"the adapter must register its own events, not extend the core catalog")
}
