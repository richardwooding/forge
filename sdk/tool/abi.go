// Package tool is the guest side of forge.
//
// A forge tool is an ordinary Go main package that registers itself from a
// package-level variable or init, and is built with
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared
//
// which produces a WASI *reactor*: the module exports _initialize and every
// //go:wasmexport function, and main is never called. That last point is the
// one that catches people out, so it is worth stating plainly: putting
// tool.Register inside func main registers nothing. Register is written to be
// used as a package-level var so that the natural spelling is the correct one,
// and forge reports the mistake by name if it happens anyway.
//
// This module deliberately depends on nothing but jsonschema-go. A tool's wasm
// binary is already a couple of megabytes of Go runtime; it must not also carry
// forge's own dependencies.
package tool

import (
	"unsafe"
)

// ABIVersion is the guest/host contract this SDK speaks. forge refuses a module
// built against an ABI it does not implement, rather than letting the mismatch
// surface later as an unexplainable failure.
const ABIVersion = 1

// The host ABI.
//
// Variable-length data moves in one direction per call, and never by the host
// calling back into the guest. To read its input the guest asks how big the
// input is, allocates an ordinary Go slice, and asks the host to copy into it;
// to return a result the guest hands back a pointer into its own memory, which
// the host reads before any further guest code runs.
//
// The alternative -- exporting an allocator for the host to call mid-host-call
// -- would mean allocating on a fresh goroutine while the calling goroutine is
// suspended, on a runtime with a single M, and would invalidate any slice the
// host was holding the moment the allocation grew linear memory. This shape
// avoids the question entirely.

//go:wasmimport forge call_len
func hostCallLen() int32

//go:wasmimport forge call_read
func hostCallRead(ptr unsafe.Pointer, capacity int32) int32

//go:wasmimport forge log
func hostLog(level int32, msg string)

//go:wasmimport forge progress
func hostProgress(done, total int64, msg string)

// pack combines a pointer and length into the i64 an export returns.
func pack(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	ptr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(b))))
	return uint64(ptr)<<32 | uint64(uint32(len(b)))
}

// retained keeps the bytes an export returned reachable until the next export
// call. The host reads them immediately after the call returns and before any
// other guest code runs, but nothing tells Go's collector that, so without this
// the slice could be collected between the return and the read.
var retained []byte

func retain(b []byte) uint64 {
	retained = b
	return pack(b)
}

// readCall fetches the pending call envelope the host has staged for this
// invocation.
func readCall() []byte {
	n := hostCallLen()
	if n <= 0 {
		return nil
	}
	buf := make([]byte, n)
	got := hostCallRead(unsafe.Pointer(unsafe.SliceData(buf)), n)
	if got < 0 || got > n {
		return nil
	}
	return buf[:got]
}
