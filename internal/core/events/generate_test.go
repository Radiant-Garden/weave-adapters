/*
Testing: generate.go

Pending:

Tested:

Tested elsewhere:
  The generator this directive runs (docgen) is unit-tested in
  internal/core/events/docgen, and CI's generate-check verifies the committed
  docs/events.md is current.

Declined:
  generate.go declares no functions — only the //go:generate comment that runs
  ./docgen. There is nothing to unit-test here.

Additional Remarks:
  This file exists solely to satisfy the one-test-file-per-source-file rule.
*/

package events
