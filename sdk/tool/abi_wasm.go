//go:build wasm

package tool

import "unsafe"

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
