/*
Testing: the core/adapter layering rule (no corresponding .go file)

Pending:

Tested:

  - TestLayering_ShouldKeepCoreFreeOfAdapterImports: no package under
    internal/core imports internal/adapters. This is the CLAUDE.md rule that
    core stays adapter-agnostic, and dhcpwindows is the first package that
    could ever break it.
  - TestLayering_ShouldPartitionTheBackendEventRange: every registered BACKEND
    event sits in this adapter's 1xx partition, so a second adapter can share the
    category without breaking the single-owner rule.

Tested elsewhere:

	The same technique applied to the test-only demo resource:
	  internal/core/httptest/demo_test.go's TestDemo_ShouldNotBeReachableFromTheBinary,
	  which is the precedent this file follows.

Declined:

	Asserting the reverse direction (that the adapter *may* import core): it does,
	  visibly, in config.go — a test would restate an import statement.
	Enforcing that the binary links the adapter: covered by the wiring itself once
	  Phase 1 lands; asserting the dependency edge adds nothing the build does not.
	Asserting the adapter does not import internal/core/events/catalog: it reaches
	  it transitively through core/health, which is legitimate. The invariant that
	  matters is which IDs exist and who owns them, asserted on the registry.

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

	adapterevents "github.com/radiantgarden/weave-adapters/internal/adapters/dhcpwindows/events"
	coreevents "github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

const modulePath = "github.com/radiantgarden/weave-adapters"

// deps returns every package the given pattern transitively depends on,
// including the imports of its test files.
//
// -test is load bearing: without it `go list -deps` reports only the production
// import graph, so a core _test.go importing the adapter would sail past the
// one mechanical guard on this repo's headline layering rule. Test code is
// still core code for this purpose — a core package that needs an adapter to
// test itself is a core package that knows about an adapter.
func deps(t *testing.T, pattern string) []string {
	t.Helper()

	//nolint:gosec // G204: pattern is a compile-time constant from this file.
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "-test", pattern).CombinedOutput()
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

func TestLayering_ShouldPartitionTheBackendEventRange(t *testing.T) {
	t.Parallel()

	// ARRANGE — importing the core catalog and this adapter's events package
	// registers both sets from init(), so the registry holds everything a real
	// binary would.
	_ = catalog.SYS001
	_ = adapterevents.BACKEND101

	// ACT
	registered := coreevents.GetAll()

	// ASSERT — the category constant is shared and lives in core, but every
	// BACKEND *event* belongs to an adapter, in a partitioned ID range. This is
	// forced by the single-owner rule: a BACKEND-001 registered in the core
	// catalog would break it the moment a second adapter wanted to emit a
	// backend failure, since both would have to emit an ID core owns.
	//
	// Asserting on the registry rather than on the import graph is deliberate.
	// The adapter reaches internal/core/events/catalog transitively through
	// core/health, which is legitimate and unavoidable, so an import-edge
	// assertion would forbid something harmless while missing the thing that
	// actually matters — which IDs exist and who may emit them.
	var backend []string

	for id, event := range registered {
		if event.Category == coreevents.CategoryBackend.String() {
			backend = append(backend, string(id))
		}
	}

	require.NotEmpty(t, backend, "the BACKEND category must have at least one live event, or it is a ghost category")

	for _, id := range backend {
		assert.Regexp(t, `^BACKEND-1\d\d$`, id,
			"BACKEND events must sit in this adapter's 1xx partition; the next adapter takes 2xx")
	}
}
