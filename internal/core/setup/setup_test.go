/*
Testing: setup.go

Pending:

Tested:

Tested elsewhere:
  Deps and the three function types are a wiring struct: they have no behaviour
  of their own, and every field is exercised through the steps that call it.
  install_test.go asserts each one is consulted — the manager factory, the
  lockdown, and the binary-directory check — and asserts what happens when each
  refuses.

  That the real operations satisfy these shapes is a compile-time fact,
  established by cmd/weave-adapter-dhcp-windows/service.go's platformDeps.

Declined:
  A test that constructs a Deps and asserts its fields are the ones assigned.
  It would restate the struct literal and fail only if the compiler had already
  failed.

Additional Remarks:
  This file exists because every .go file has one, and because the reason there
  is nothing to assert here is itself worth recording: if Deps ever grows a
  method — a default, a validation, a fallback for a nil field — it stops being
  a wiring struct and this block is where the coverage claim above becomes
  wrong.
*/

package setup
