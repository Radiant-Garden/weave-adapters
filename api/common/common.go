// Package common holds the OpenAPI components every adapter shares, and the Go
// types generated from them.
//
// The files here carry no paths — they are a component library, so /openapi.yaml
// still answers 404 and M3 swaps in the adapter's own spec. That is why each
// config sets skip-prune: with no operations referencing them, oapi-codegen
// would treat every schema as dead and emit an empty file.
//
// Models only. Server stubs need paths, and there are none until M3.
//
// apierror.Problem and pagination.Page already exist in Go, so common_test.go
// compares the generated types against them field by field. Two definitions of
// one wire format is the drift this package exists to prevent.
package common

//go:generate go tool oapi-codegen -config errors.cfg.yaml errors.yaml
//go:generate go tool oapi-codegen -config pagination.cfg.yaml pagination.yaml
//go:generate go tool oapi-codegen -config jobs.cfg.yaml jobs.yaml
