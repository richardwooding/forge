//go:build wasm

package tool

import (
	"runtime"
	"unsafe"
)

// The host ABI, declared only for wasm targets: a //go:wasmimport function has
// no body, so these declarations cannot compile anywhere else. abi_host.go
// supplies stubs for every other target, which is what lets the SDK's own logic
// be built, vetted and unit-tested on an ordinary machine.

//go:wasmimport forge call_len
func hostCallLen() int32

//go:wasmimport forge call_read
func hostCallRead(ptr unsafe.Pointer, capacity int32) int32

//go:wasmimport forge log
func hostLog(level int32, msg string)

//go:wasmimport forge progress
func hostProgress(done, total int64, msg string)

// The capability-backed calls. Each takes a JSON request in linear memory and
// returns handle<<32|len naming a staged result; result_fetch then copies that
// result into a buffer the guest allocated. Two calls rather than one because
// the alternative -- the host calling a guest-exported allocator -- would run
// guest code while this goroutine is suspended, on wasm's single M, and any
// allocation can grow linear memory and move what the host is holding.
//
// Every one is imported unconditionally. The host registers all four whether
// or not the tool holds the capability, so a single build of a tool
// instantiates under any grant set and a denial arrives as a value.

//go:wasmimport forge http
func hostHTTP(ptr unsafe.Pointer, n int32) uint64

//go:wasmimport forge kv
func hostKV(ptr unsafe.Pointer, n int32) uint64

//go:wasmimport forge secret
func hostSecret(ptr unsafe.Pointer, n int32) uint64

//go:wasmimport forge invoke
func hostInvoke(ptr unsafe.Pointer, n int32) uint64

//go:wasmimport forge result_fetch
func hostResultFetch(handle int32, ptr unsafe.Pointer, capacity int32) int32

// hostCall performs one size-then-fetch round trip and returns the envelope
// the host staged.
func hostCall(svc service, req []byte) ([]byte, error) {
	if len(req) == 0 {
		return nil, errHostCall
	}
	packed := callService(svc, unsafe.Pointer(unsafe.SliceData(req)), int32(len(req)))
	runtime.KeepAlive(req)

	handle := int32(packed >> 32)
	length := int32(uint32(packed))
	if handle == 0 {
		// The host failed before it had anything to stage -- a request too
		// large to read, or a full result table.
		return nil, errHostCall
	}
	if length < 0 {
		return nil, errHostCall
	}

	buf := make([]byte, length)
	n := hostResultFetch(handle, unsafe.Pointer(unsafe.SliceData(buf)), length)
	runtime.KeepAlive(buf)
	if n < 0 || n > length {
		return nil, errHostCall
	}
	return buf[:n], nil
}

func callService(svc service, ptr unsafe.Pointer, n int32) uint64 {
	switch svc {
	case svcHTTP:
		return hostHTTP(ptr, n)
	case svcKV:
		return hostKV(ptr, n)
	case svcSecret:
		return hostSecret(ptr, n)
	case svcInvoke:
		return hostInvoke(ptr, n)
	}
	return 0
}

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

// readCall fetches the call envelope the host staged for this invocation. The
// guest allocates and the host copies in, so the host never calls back into the
// guest to allocate.
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

//go:wasmexport forge_describe
func forgeDescribe() uint64 { return retain(describe()) }

//go:wasmexport forge_invoke
func forgeInvoke() uint64 { return retain(invoke(readCall())) }
