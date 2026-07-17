/*
Testing: main.go

Pending:

Tested:

Tested elsewhere:

Declined:
  run / main — thin orchestration over config.Load, observability.Setup,
  httpserver.New/Run and the SYS-001/005 emissions, each tested in its own
  package. A unit test here would re-exercise already-covered components and bind
  to process-global state (signal handling, the slog default), so it is
  intentionally left untested — standard for a main wiring function.

Additional Remarks:
  File added to satisfy the one-test-file-per-source-file rule.
*/

package main
