//go:build !wasm

package tool

// Stubs for non-wasm builds.
//
// A tool is only useful compiled to wasip1, but the SDK's own logic -- building
// the manifest, dispatching an operation, encoding a result -- is ordinary Go.
// Keeping it buildable on the host means the usual tooling can vet it and its
// behaviour can be unit-tested without a wasm runtime. The host ABI is the only
// part that genuinely cannot exist here.
//
// These are silent no-ops rather than panics: a handler that logs should not
// explode merely because its package was built for a test binary.
//
// Only the two a handler can reach are stubbed. readCall and retain exist
// purely to serve the //go:wasmexport entry points, which do not exist off
// wasm, so stubbing them here would be dead code on every host build.

func hostLog(level int32, msg string)            {}
func hostProgress(done, total int64, msg string) {}
