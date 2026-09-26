package hostabi

import (
	"context"
	"encoding/binary"
	"sync"

	"github.com/tetratelabs/wazero/api"
)

// Status is how a host call ended, carried in the envelope so that a denial
// and a failure are distinguishable by the guest without parsing a message.
type Status uint8

const (
	// StatusOK means the body is the result.
	StatusOK Status = 0
	// StatusDenied means the capability was refused. The body is the reason,
	// which the guest can show or act on -- a tool that can see it was refused
	// the network can fall back rather than simply failing.
	StatusDenied Status = 1
	// StatusError means the host could not complete the call.
	StatusError Status = 2
)

// envelopeHeader is the fixed prefix on every host-call result.
//
//	0      version
//	1      status
//	2..3   reserved
//	4..7   body length, little endian
const envelopeHeader = 8

// envelopeVersion is the framing this host writes.
const envelopeVersion = 1

func envelope(status Status, body []byte) []byte {
	out := make([]byte, envelopeHeader+len(body))
	out[0] = envelopeVersion
	out[1] = byte(status)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(body)))
	copy(out[envelopeHeader:], body)
	return out
}

// results holds payloads a guest has been told about but not yet fetched.
//
// The two-call shape exists so that the host never calls back into the guest
// to allocate. An exported allocator would mean allocating on a fresh
// goroutine while the caller's goroutine is suspended, on a runtime with a
// single M, and any allocation that grew linear memory would move a slice the
// host was still holding.
type results struct {
	mu     sync.Mutex
	next   uint32
	byID   map[uint32][]byte
	closed bool
}

// maxLiveResults caps how many unfetched payloads one invocation may hold.
//
// A guest that calls without ever fetching would otherwise grow the host's
// heap for free; this turns that into an ordinary error at a bounded cost.
const maxLiveResults = 64

func newResults() *results { return &results{byID: map[uint32][]byte{}} }

// put stores a payload and returns its handle, or 0 when there are too many
// outstanding.
func (r *results) put(b []byte) uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || len(r.byID) >= maxLiveResults {
		return 0
	}
	r.next++
	if r.next == 0 {
		r.next = 1 // zero means failure, so it is never a valid handle
	}
	id := r.next
	r.byID[id] = b
	return id
}

// take removes and returns a payload.
func (r *results) take(id uint32) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.byID[id]
	if ok {
		delete(r.byID, id)
	}
	return b, ok
}

// peek reports a payload's length without removing it, for the guest to size
// its buffer.
func (r *results) peek(id uint32) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.byID[id]
	return len(b), ok
}

// release drops everything, called when an invocation ends so that a guest
// which abandoned a fetch cannot leave the payload behind.
func (r *results) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.byID = map[uint32][]byte{}
}

// pack combines a handle and a length into the i64 a host function returns.
// A zero handle means the call failed before producing anything.
func pack(handle uint32, length int) uint64 {
	return uint64(handle)<<32 | uint64(uint32(length))
}

// service is one host call: request bytes in, result bytes and a status out.
type service func(ctx context.Context, inv *Invocation, req []byte) (Status, []byte)

// serve wires a service into a wazero host function.
//
// Every call goes through here so that the three rules hold uniformly: bound
// the length before trusting it, copy out of linear memory immediately, and
// never panic. A panic in a host function becomes a wasm trap that unwinds the
// guest with nothing to report.
func (m *hostModule) serve(fn service) api.GoModuleFunc {
	return func(ctx context.Context, mod api.Module, stack []uint64) {
		defer guard()

		inv := From(ctx)
		if inv == nil {
			stack[0] = 0
			return
		}

		req, ok := readGuestLimited(inv, mod, api.DecodeU32(stack[0]), api.DecodeI32(stack[1]),
			inv.Limits.MaxHostCallBytes)
		if !ok {
			stack[0] = 0
			return
		}

		status, body := fn(ctx, inv, req)
		env := envelope(status, body)

		id := inv.results.put(env)
		if id == 0 {
			stack[0] = 0
			return
		}
		stack[0] = pack(id, len(env))
	}
}

// resultFetch copies a payload into a guest-provided buffer and frees it.
func resultFetch(ctx context.Context, mod api.Module, stack []uint64) {
	defer guard()

	inv := From(ctx)
	if inv == nil {
		stack[0] = api.EncodeI32(-1)
		return
	}
	id := api.DecodeU32(stack[0])
	ptr := api.DecodeU32(stack[1])
	capacity := api.DecodeI32(stack[2])

	n, ok := inv.results.peek(id)
	if !ok {
		stack[0] = api.EncodeI32(-1)
		return
	}
	if capacity < 0 || int(capacity) < n {
		// Too small. The payload is left in place so the guest can retry with
		// a correct buffer rather than losing the result to its own mistake.
		stack[0] = api.EncodeI32(-2)
		return
	}

	body, ok := inv.results.take(id)
	if !ok {
		stack[0] = api.EncodeI32(-1)
		return
	}
	if !mod.Memory().Write(ptr, body) {
		stack[0] = api.EncodeI32(-1)
		return
	}
	stack[0] = api.EncodeI32(int32(len(body)))
}

// readGuestLimited reads a request, bounded by an explicit cap.
func readGuestLimited(inv *Invocation, mod api.Module, ptr uint32, length int32, max int64) ([]byte, bool) {
	if length < 0 {
		return nil, false
	}
	if int64(length) > max {
		inv.truncated.Store(true)
		return nil, false
	}
	if !inv.charge(int64(length)) {
		return nil, false
	}
	if length == 0 {
		return nil, true
	}
	raw, ok := mod.Memory().Read(ptr, uint32(length))
	if !ok {
		return nil, false
	}
	// Copy before anything can grow linear memory and move the backing array.
	out := make([]byte, len(raw))
	copy(out, raw)
	return out, true
}
