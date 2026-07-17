/*
Testing: main.go

Pending:

Tested:
  render
    - TestRender_ShouldListRegisteredEventsSortedByID: the header, every SYS event,
      and ID-sorted ordering.
  writeEvent
    - TestWriteEvent_ShouldRenderAllSections: level, category/topic, external-source
      line, field table, example, and troubleshooting.
  moduleRoot
    - TestModuleRoot_ShouldFindGoMod: walks up to the directory holding go.mod.

Tested elsewhere:
  main: the os.Exit / filesystem-writing entrypoint is exercised end-to-end by
    CI's generate-check; its logic beyond render() is I/O we do not unit-test.

Declined:

Additional Remarks:
  render() reads the global registry, which docgen populates via its blank import
  of the catalog package, so the SYS events are present without extra setup.
*/

package main

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/radiantgarden/weave-adapters/internal/core/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRender_ShouldListRegisteredEventsSortedByID(t *testing.T) {
	t.Parallel()

	// ACT
	out := render()

	// ASSERT
	assert.True(t, strings.HasPrefix(out, "# Event Catalog\n"), "output should start with the catalog header")

	for _, id := range []string{"SYS-001", "SYS-002", "SYS-003", "SYS-004", "SYS-005"} {
		assert.Contains(t, out, "## "+id, "catalog should list %s", id)
	}

	// Sorted by ID: SYS-001's heading precedes SYS-002's.
	assert.Less(t, strings.Index(out, "## SYS-001"), strings.Index(out, "## SYS-002"))
}

func TestWriteEvent_ShouldRenderAllSections(t *testing.T) {
	t.Parallel()

	// ARRANGE
	var b strings.Builder

	// ACT
	writeEvent(&b, &events.Event{
		ID:              "DOC-001",
		Level:           slog.LevelInfo,
		MessageTemplate: "msg",
		Category:        "SYS",
		Topic:           "Lifecycle",
		Description:     "desc",
		ExternalSource:  true,
		Fields:          []events.FieldDef{{Name: "version", Type: "string", Required: true, Description: "v"}},
		Example:         `{"eventId":"DOC-001"}`,
		Troubleshooting: "do x",
	})
	out := b.String()

	// ASSERT
	assert.Contains(t, out, "## DOC-001 — msg")
	assert.Contains(t, out, "- **Level:** INFO")
	assert.Contains(t, out, "- **Category / Topic:** SYS / Lifecycle")
	assert.Contains(t, out, "- **External source:** yes")
	assert.Contains(t, out, "- **Description:** desc")
	assert.Contains(t, out, "| version | string | true | v |")
	assert.Contains(t, out, "**Example:** `{\"eventId\":\"DOC-001\"}`")
	assert.Contains(t, out, "**Troubleshooting:** do x")
}

func TestModuleRoot_ShouldFindGoMod(t *testing.T) {
	t.Parallel()

	// ACT
	root, err := moduleRoot()

	// ASSERT
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(root, "go.mod"))
}
