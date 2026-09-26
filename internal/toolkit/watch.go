package toolkit

import (
	"context"
	"time"

	"github.com/richardwooding/forge/internal/core"
	"github.com/richardwooding/forge/internal/statefile"
)

// WatchInterval is how often a watcher looks for changes.
//
// forge's state changes at human speed -- someone runs `forge tool add`, or
// `forge view set` -- so a second is already far quicker than anyone can
// notice, and the check is a stat of one directory. An inotify watch would buy
// nothing here and would bring platform differences and a queue that can
// overflow.
const WatchInterval = time.Second

// Watch reports when the installed tools or the saved views change underneath
// this process.
//
// Each call gets its own channel, so several surfaces can watch at once
// without starving each other, and the channel closes when ctx is done. The
// events carry EventResync rather than per-tool additions and removals: every
// consumer already has to diff against its own idea of the world to know what
// to add and remove, so computing a precise delta here would only be done
// twice.
func (tk *Toolkit) Watch(ctx context.Context) <-chan core.Event {
	out := make(chan core.Event, 1)

	go func() {
		defer close(out)

		tools := tk.store.Stamp()
		views := statefile.Of(tk.views.Path())

		ticker := time.NewTicker(WatchInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			nextTools, nextViews := tk.store.Stamp(), statefile.Of(tk.views.Path())
			if !statefile.Changed(tools, nextTools) && !statefile.Changed(views, nextViews) {
				continue
			}
			tools, views = nextTools, nextViews

			select {
			case out <- core.Event{Kind: core.EventResync}:
			case <-ctx.Done():
				return
			default:
				// A resync already pending says everything the next one would.
				// Dropping it is the point of the buffer: a slow consumer must
				// not make the watcher block, and it cannot miss anything,
				// because the event it already has means "re-read everything".
			}
		}
	}()

	return out
}
