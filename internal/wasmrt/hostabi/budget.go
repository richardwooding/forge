package hostabi

import "sync/atomic"

// Budget is the allowance a whole call tree draws on.
//
// It is shared by pointer down every tool-to-tool call, and that sharing is
// the point. Counters living on one Invocation would be spent per node, so a
// limit of 64 calls at a depth of 8 would permit 64^8 invocations rather than
// 64 -- fanout multiplies where depth only adds. One Budget for the tree is
// the only shape that bounds the work a single surface request can cause.
//
// A tree that starts at a surface gets a fresh Budget; a nested call inherits
// the one it was handed and never replaces it.
type Budget struct {
	invokes   atomic.Int64
	httpCalls atomic.Int64
}

// Note that the bytes moved across the host boundary are NOT budgeted here.
// That limit bounds what one guest can push through the ABI, which is a
// property of that guest rather than of the tree, and lives on Invocation.

// NewBudget returns a fresh allowance for one call tree.
func NewBudget() *Budget { return &Budget{} }

// spend takes one unit from a counter, reporting whether it was within the
// limit.
//
// A limit of zero or less refuses, rather than meaning "unlimited". Limits
// reach here already defaulted, so a zero at this point is a caller that asked
// for none of something -- and a sandbox that reads "no allowance" as "any
// amount" is the wrong way round.
func spend(c *atomic.Int64, limit int64) bool {
	if limit <= 0 {
		return false
	}
	return c.Add(1) <= limit
}

// Invokes reports how many tool-to-tool calls the tree has made.
func (b *Budget) Invokes() int64 { return b.invokes.Load() }

// HTTPCalls reports how many HTTP requests the tree has made.
func (b *Budget) HTTPCalls() int64 { return b.httpCalls.Load() }
