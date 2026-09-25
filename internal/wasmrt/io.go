package wasmrt

import (
	"bytes"
	"crypto/rand"
	"io"
	"sync"
)

// cryptoRand is the randomness source handed to every guest.
//
// It is not gated behind a capability. wazero's default source is
// deterministic, which inside a Go guest means a predictable map hash seed --
// so withholding randomness would not restrict a tool, it would weaken it.
// Determinism belongs behind an explicit replay mode, not behind a grant.
type cryptoRand struct{}

func (cryptoRand) Read(p []byte) (int, error) { return rand.Read(p) }

func newBytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// CappedBuffer collects guest output up to a limit and records whether more was
// produced.
//
// Reporting truncation matters as much as enforcing the cap: a surface that
// silently presented the first megabyte of a larger answer as the whole answer
// would be worse than one that refused it.
type CappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	written   int
	truncated bool
}

// NewCappedBuffer returns a buffer that stops collecting after limit bytes.
func NewCappedBuffer(limit int) *CappedBuffer { return &CappedBuffer{limit: limit} }

func (c *CappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written += len(p)
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	// Always report the full length written. Returning a short count would
	// make the guest's own io.Writer contract report an error, turning forge's
	// output cap into a write failure inside the tool.
	return len(p), nil
}

// Bytes returns what was collected.
func (c *CappedBuffer) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

// Truncated reports whether output was dropped.
func (c *CappedBuffer) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

// Written reports how many bytes the guest produced, including dropped ones.
func (c *CappedBuffer) Written() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written
}
