// Command since remembers what it has already been shown, so that a caller
// running the same check twice only hears about what changed.
//
// It does no fetching. Whatever produces the items -- a feed, a CI run, a
// directory listing, another forge tool -- hands them here and gets back the
// subset that is new. Keeping the remembering separate from the fetching is
// what makes it reusable: one memory, any source.
//
// Two shapes, because "what is new" means two different things in practice.
// A set of identifiers, where anything unseen is new, suits feeds and issue
// lists. A watermark, where anything greater than the last value is new, suits
// timestamps and sequence numbers and costs one stored value instead of
// thousands.
package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/richardwooding/forge/sdk/tool"
)

// namespace is this tool's private corner of forge's key/value store. A
// namespace belongs to one tool, so nothing else can read or tread on it.
const namespace = "since"

// maxRemembered caps how many identifiers one stream keeps.
//
// The store caps a value at 1 MiB, and an unbounded set would eventually hit
// that and start failing -- at which point every item would look new, which is
// the worst possible failure for this tool. Evicting the oldest instead keeps
// it working, at the documented cost below.
const maxRemembered = 2000

type NewArgs struct {
	Stream string   `json:"stream" jsonschema:"a name for this source, e.g. 'ci' or 'blog'"`
	Items  []string `json:"items" jsonschema:"identifiers for the things you are looking at now"`
	Peek   bool     `json:"peek,omitempty" jsonschema:"report what is new without recording it"`
}

type NewOut struct {
	New   []string `json:"new" jsonschema:"the items not seen before, in the order given"`
	Count int      `json:"count"`
	// Recorded says whether the stream was updated. False under peek, and the
	// distinction matters: a caller that crashes after a recording call has
	// lost those items, whereas one that peeked can safely try again.
	Recorded bool `json:"recorded"`
	// Remembered is how many identifiers the stream now holds.
	Remembered int `json:"remembered"`
	// Evicted reports that the oldest identifiers were dropped to stay within
	// the cap, so a very old item reappearing would read as new again.
	Evicted int `json:"evicted,omitempty"`
	First   bool `json:"first,omitempty" jsonschema:"true when this stream had never been seen, so everything looked new"`
}

type AfterArgs struct {
	Stream string `json:"stream" jsonschema:"a name for this source"`
	Value  string `json:"value" jsonschema:"a sortable value: a timestamp, a sequence number, a version"`
	Peek   bool   `json:"peek,omitempty" jsonschema:"compare without recording"`
}

type AfterOut struct {
	Newer    bool   `json:"newer"`
	Previous string `json:"previous,omitempty" jsonschema:"the value remembered before this call"`
	Recorded bool   `json:"recorded"`
	First    bool   `json:"first,omitempty"`
}

type StreamArgs struct {
	Stream string `json:"stream" jsonschema:"the stream to forget"`
}

type ForgetOut struct {
	Forgotten bool `json:"forgotten" jsonschema:"false when there was nothing to forget"`
}

type StreamsOut struct {
	Streams []Stream `json:"streams"`
}

type Stream struct {
	Name       string `json:"name"`
	Kind       string `json:"kind" jsonschema:"set or watermark"`
	Remembered int    `json:"remembered,omitempty"`
	Value      string `json:"value,omitempty"`
	Updated    string `json:"updated,omitempty"`
}

var _ = tool.Register(
	tool.Spec{
		Name:    "since",
		Version: "0.1.0",
		Summary: "Remember what you have already seen, and report only what is new",
		Description: "Give it a stream name and the things you are looking at; it returns " +
			"the ones it has not seen before. Use it to check a feed, a build queue or an " +
			"inbox repeatedly without re-reporting what you already handled.",
		Labels: []string{"state", "memory", "watch"},
		Needs: []tool.Need{
			{
				Kind:   tool.KV,
				Scope:  []string{namespace},
				Reason: "remember which items it has already shown you, between runs",
			},
			{
				Kind:  tool.ClockWall,
				Scope: []string{"*"},
				Reason: "record when each stream was last updated. Without it forge gives " +
					"tools a frozen clock, and every stream would claim to have been " +
					"updated on the same day in 2022",
			},
		},
	},
	tool.Op("new", newItems,
		tool.Summary("Which of these have I not seen before?"),
		tool.OpenWorld()),
	tool.Op("after", after,
		tool.Summary("Is this value newer than the last one I recorded?"),
		tool.OpenWorld()),
	tool.Op("forget", forget,
		tool.Summary("Forget a stream, so everything looks new again"),
		tool.Destructive()),
	tool.Op("streams", streams,
		tool.Summary("What is being remembered"),
		tool.ReadOnly()),
)

func main() {}

// setRecord is a stream remembered as a set of identifiers.
type setRecord struct {
	// IDs is ordered oldest first, which is what makes eviction possible.
	IDs     []string `json:"ids"`
	Updated string   `json:"updated"`
}

// markRecord is a stream remembered as a single high-water mark.
type markRecord struct {
	Value   string `json:"value"`
	Updated string `json:"updated"`
}

func setKey(stream string) string  { return "set:" + stream }
func markKey(stream string) string { return "mark:" + stream }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func checkStream(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a stream name is required; it is how this tool tells one source from another")
	}
	if strings.ContainsAny(name, ":") {
		return fmt.Errorf("a stream name may not contain ':', which separates the kind from the name in storage")
	}
	return nil
}

func newItems(ctx *tool.Context, a NewArgs) (NewOut, error) {
	if err := checkStream(a.Stream); err != nil {
		return NewOut{}, err
	}

	store := tool.OpenKV(namespace)
	var rec setRecord
	found, err := store.GetJSON(setKey(a.Stream), &rec)
	if err != nil {
		return NewOut{}, err
	}

	seen := make(map[string]bool, len(rec.IDs))
	for _, id := range rec.IDs {
		seen[id] = true
	}

	// Order is the caller's, so the answer reads in the same order as what was
	// handed over. Duplicates within one call are reported once.
	//
	// Initialised rather than left nil so that "nothing new" serialises as []
	// and not null. A caller checking the length of the result should not have
	// to special-case the commonest outcome.
	fresh := []string{}
	inThisCall := make(map[string]bool, len(a.Items))
	for _, id := range a.Items {
		if id == "" || seen[id] || inThisCall[id] {
			continue
		}
		inThisCall[id] = true
		fresh = append(fresh, id)
	}

	out := NewOut{
		New:        fresh,
		Count:      len(fresh),
		First:      !found,
		Remembered: len(rec.IDs),
	}
	if a.Peek || len(fresh) == 0 {
		// Nothing new means nothing to write. Skipping the write keeps a
		// polling caller from rewriting the same value every minute.
		out.Remembered = len(rec.IDs)
		return out, nil
	}

	rec.IDs = append(rec.IDs, fresh...)
	if over := len(rec.IDs) - maxRemembered; over > 0 {
		rec.IDs = rec.IDs[over:]
		out.Evicted = over
	}
	rec.Updated = now()
	if err := store.SetJSON(setKey(a.Stream), rec); err != nil {
		return NewOut{}, err
	}

	out.Recorded = true
	out.Remembered = len(rec.IDs)
	return out, nil
}

func after(ctx *tool.Context, a AfterArgs) (AfterOut, error) {
	if err := checkStream(a.Stream); err != nil {
		return AfterOut{}, err
	}
	if a.Value == "" {
		return AfterOut{}, fmt.Errorf("a value is required")
	}

	store := tool.OpenKV(namespace)
	var rec markRecord
	found, err := store.GetJSON(markKey(a.Stream), &rec)
	if err != nil {
		return AfterOut{}, err
	}

	// String comparison, which is right for RFC 3339 timestamps and for
	// zero-padded sequence numbers, and wrong for unpadded ones: "9" sorts
	// after "10". Said plainly here rather than guessed at by parsing, since
	// guessing wrong would silently skip items.
	newer := !found || a.Value > rec.Value

	out := AfterOut{Newer: newer, Previous: rec.Value, First: !found}
	if a.Peek || !newer {
		return out, nil
	}

	rec.Value = a.Value
	rec.Updated = now()
	if err := store.SetJSON(markKey(a.Stream), rec); err != nil {
		return AfterOut{}, err
	}
	out.Recorded = true
	return out, nil
}

func forget(ctx *tool.Context, a StreamArgs) (ForgetOut, error) {
	if err := checkStream(a.Stream); err != nil {
		return ForgetOut{}, err
	}
	store := tool.OpenKV(namespace)

	var had bool
	for _, key := range []string{setKey(a.Stream), markKey(a.Stream)} {
		if _, found, err := store.Get(key); err != nil {
			return ForgetOut{}, err
		} else if found {
			had = true
		}
		if err := store.Delete(key); err != nil {
			return ForgetOut{}, err
		}
	}
	return ForgetOut{Forgotten: had}, nil
}

func streams(ctx *tool.Context, _ struct{}) (StreamsOut, error) {
	store := tool.OpenKV(namespace)
	keys, err := store.List("")
	if err != nil {
		return StreamsOut{}, err
	}
	sort.Strings(keys)

	out := StreamsOut{Streams: []Stream{}}
	for _, key := range keys {
		switch {
		case strings.HasPrefix(key, "set:"):
			var rec setRecord
			if _, err := store.GetJSON(key, &rec); err != nil {
				return StreamsOut{}, err
			}
			out.Streams = append(out.Streams, Stream{
				Name: strings.TrimPrefix(key, "set:"), Kind: "set",
				Remembered: len(rec.IDs), Updated: rec.Updated,
			})
		case strings.HasPrefix(key, "mark:"):
			var rec markRecord
			if _, err := store.GetJSON(key, &rec); err != nil {
				return StreamsOut{}, err
			}
			out.Streams = append(out.Streams, Stream{
				Name: strings.TrimPrefix(key, "mark:"), Kind: "watermark",
				Value: rec.Value, Updated: rec.Updated,
			})
		}
	}
	return out, nil
}
