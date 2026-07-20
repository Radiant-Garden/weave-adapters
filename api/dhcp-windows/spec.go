// Package dhcpwindows holds the served OpenAPI contract for
// weave-adapter-dhcp-windows and the Go types generated from it.
//
// The document is embedded rather than read from disk so the release artifact
// stays a single file — the adapter ships as one .exe onto a Windows host, and
// a spec that had to be deployed beside it would eventually be deployed
// without it.
//
// The bytes travel to the server as an option (httpserver.WithOpenAPISpec)
// rather than being imported by it. internal/core must never import an
// adapter, and the spec is adapter content; passing it inward is the same
// shape as passing health probes in.
//
// Models only. The server interface is deliberately not generated yet: with no
// handler to implement it, the output would be a ghost. When it lands with the
// scopes handler it is the plain stdlib ServerInterface, never strict-server —
// strict-server wants handlers that return a generated response union, which
// fights this repo's rule that handlers return an error and apierror.WriteError
// is the one place that logs and responds. Adopting it would mean either
// bypassing the generated layer or growing a second error-rendering path.
//
// The generated Scope and the hand-written internal/adapters/dhcpwindows.Scope
// are two descriptions of one wire format, held together by spec_test.go.
package dhcpwindows

import (
	"bytes"
	_ "embed"
)

//go:generate go tool oapi-codegen -config openapi.cfg.yaml openapi.yaml

// specBytes is the embedded document. Unexported because an exported []byte is
// writable by anything in the process, and this one is the API contract the
// binary serves — a stray write would change what every subsequent request
// returns, with nothing to detect it.
//
//go:embed openapi.yaml
var specBytes []byte

// Spec returns the served OpenAPI document, exactly as it sits on disk.
//
// Serving the file rather than a re-serialization is what keeps the contract a
// client reads identical to the one a reviewer reads: a round trip through a
// YAML library would reorder keys and drop every comment explaining why a field
// is shaped the way it is.
//
// It returns a copy. The cost is one 18 KB allocation at startup, where the
// binary calls this once; the alternative is handing every caller a mutable
// alias of the contract.
func Spec() []byte {
	return bytes.Clone(specBytes)
}
