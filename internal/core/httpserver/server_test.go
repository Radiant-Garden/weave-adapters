/*
Testing: server.go

Pending:

Tested:

Tested elsewhere:

Declined:
  Run / New — deferred to Phase 6. Run's goroutine + select + graceful Shutdown
  and its SYS-002/003/004 emissions are walking-skeleton code slated for a Phase 6
  rewrite (metrics, middleware, configurable timeouts). The serve-then-cancel
  drain test will land with that rewrite rather than against code about to change.

Additional Remarks:
  File added to satisfy the one-test-file-per-source-file rule while the server
  tests are deferred.
*/

package httpserver
