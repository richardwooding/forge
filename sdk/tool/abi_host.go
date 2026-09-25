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

func hostLog(level int32, msg string)            {}
func hostProgress(done, total int64, msg string) {}
func readCall() []byte                           { return nil }
func retain(b []byte) uint64                     { return 0 }
