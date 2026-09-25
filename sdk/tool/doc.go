// Package tool is the guest side of forge.
//
// A forge tool is an ordinary Go main package that registers itself from a
// package-level variable, and is built with
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared
//
// which produces a WASI *reactor*: the module exports _initialize and every
// //go:wasmexport function, and main is never called. That last point catches
// people out, so it is worth stating plainly: putting tool.Register inside func
// main registers nothing. Register is written to be used as a package-level var
// so that the natural spelling is the correct one, and forge names the mistake
// if it happens anyway.
//
// This module deliberately depends on nothing but jsonschema-go. A tool's wasm
// binary already carries a couple of megabytes of Go runtime; it must not also
// carry forge's own dependencies.
package tool

// ABIVersion is the guest/host contract this SDK speaks. forge refuses a module
// built against an ABI it does not implement, rather than letting the mismatch
// surface later as an unexplainable failure.
const ABIVersion = 1
