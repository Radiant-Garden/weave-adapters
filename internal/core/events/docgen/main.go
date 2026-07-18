// Command docgen walks the event registry and writes the operator-facing event
// catalog to docs/events.md at the module root. It is invoked via
// `go generate ./...` (see ../generate.go); CI's generate-check fails if the
// committed file is stale.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/radiantgarden/weave-adapters/internal/core/events"

	// Blank import so the catalog's init() registers every event before we
	// walk the registry.
	_ "github.com/radiantgarden/weave-adapters/internal/core/events/catalog"
)

// problemTypePrefix mirrors apierror's namespace. It is duplicated rather than
// imported because apierror imports the catalog, and docgen importing apierror
// would close that loop.
const problemTypePrefix = "weave-adapters:"

func main() {
	root, err := moduleRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}

	out := filepath.Join(root, "docs", "events.md")

	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}

	if err := os.WriteFile(out, []byte(render()), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}

	fmt.Println("docgen: wrote", out)
}

// moduleRoot walks up from the working directory to the directory holding go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}

		dir = parent
	}
}

// render produces the markdown catalog, sorted by event ID for a stable diff.
func render() string {
	all := events.GetAll()

	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, string(id))
	}

	sort.Strings(ids)

	var b strings.Builder

	b.WriteString("# Event Catalog\n")

	for _, id := range ids {
		writeEvent(&b, all[events.EventID(id)])
	}

	return b.String()
}

func writeEvent(b *strings.Builder, e *events.Event) {
	fmt.Fprintf(b, "\n## %s — %s\n\n", e.ID, e.MessageTemplate)
	fmt.Fprintf(b, "- **Level:** %s\n", e.Level)
	fmt.Fprintf(b, "- **Category / Topic:** %s / %s\n", e.Category, e.Topic)

	if e.ExternalSource {
		b.WriteString("- **External source:** yes\n")
	}

	fmt.Fprintf(b, "- **Description:** %s\n", e.Description)

	if len(e.Fields) > 0 {
		b.WriteString("\n| Field | Type | Required | Description |\n|---|---|---|---|\n")

		for _, f := range e.Fields {
			fmt.Fprintf(b, "| %s | %s | %t | %s |\n", f.Name, f.Type, f.Required, f.Description)
		}
	}

	writeResponse(b, e)

	if e.Example != "" {
		fmt.Fprintf(b, "\n**Example:** `%s`\n", e.Example)
	}

	if e.Troubleshooting != "" {
		fmt.Fprintf(b, "\n**Troubleshooting:** %s\n", e.Troubleshooting)
	}
}

// writeResponse documents the client-facing half of an event-derived error.
//
// Without this the catalog only describes the log line, which undercuts the
// point of the design: the same entry defines what the operator sees and what
// the client receives, and this file is where an integrator looks up either.
func writeResponse(b *strings.Builder, e *events.Event) {
	if e.ResponseCode == "" {
		return
	}

	b.WriteString("\n**Client response**\n\n")
	fmt.Fprintf(b, "- **Problem type:** `%s%s`\n", problemTypePrefix, e.ResponseCode)

	if e.ResponseDetail != "" {
		fmt.Fprintf(b, "- **Detail:** %s\n", e.ResponseDetail)
	}

	if len(e.Impacts) > 0 {
		impacts := make([]string, 0, len(e.Impacts))
		for _, impact := range e.Impacts {
			impacts = append(impacts, "`"+impact.String()+"`")
		}

		fmt.Fprintf(b, "- **Impacts:** %s\n", strings.Join(impacts, ", "))
	}
}
