/*
Testing: health.go

Pending:

Tested:

Tested elsewhere:

Declined:
  ServeHTTP / overallStatus / rank / httpStatus / NewHandler — deferred to Phase 5
  (health milestone). The worst-of-wins aggregation matrix and the 200/503 status
  mapping (the contract weave's health client keys on) will get a table test then.
  Today the only component is a hard-coded healthy core, so there is nothing but
  constants to assert.

Additional Remarks:
  File added to satisfy the one-test-file-per-source-file rule while the health
  tests are deferred.
*/

package health
