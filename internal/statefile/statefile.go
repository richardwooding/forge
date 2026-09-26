// Package statefile detects that a file or directory forge keeps its state in
// has been changed by someone else.
//
// forge is routinely several processes at once: a long-lived `forge mcp`
// serving an agent while a person runs `forge grant allow` or `forge tool add`
// in a terminal. Anything that caches on-disk state in memory will otherwise
// go on answering from a snapshot taken at startup, and the two processes
// silently disagree for as long as the server stays up.
//
// The detector is deliberately a stamp rather than a watch. Polling a stat is
// a few microseconds, works identically on every platform, and cannot miss an
// event the way an inotify queue can when it overflows -- and forge's state
// changes at human speed, so there is nothing to gain from being told sooner.
package statefile

import (
	"os"
	"sort"
	"strings"
	"time"
)

// Stamp identifies the contents of a file or directory cheaply.
//
// Modification time alone is too coarse: a grant and a revoke, or two tools
// installed in quick succession, can land inside one filesystem timestamp
// tick. Size is carried alongside it so that a same-second change of length is
// still noticed, and for a directory the entry names are folded in, so a file
// appearing or disappearing shows up even when the totals happen to match.
type Stamp struct {
	mod   time.Time
	size  int64
	names string
	count int
}

// Zero is the stamp of something that does not exist.
var Zero = Stamp{}

// Exists reports whether the stamp describes anything at all.
func (s Stamp) Exists() bool { return s != Zero }

// Of stamps one file. A missing file stamps as Zero rather than as an error:
// "not there yet" is an ordinary state for a config forge has never written.
func Of(path string) Stamp {
	fi, err := os.Stat(path)
	if err != nil {
		return Zero
	}
	return Stamp{mod: fi.ModTime(), size: fi.Size(), count: 1}
}

// OfDir stamps the files directly inside dir whose names end in suffix.
//
// It does not recurse. Everything forge stamps this way is a flat directory of
// records, and walking a tree would make the cost depend on how much is stored
// rather than on how much is being watched.
func OfDir(dir, suffix string) Stamp {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Zero
	}
	var (
		s     Stamp
		names []string
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		names = append(names, e.Name())
		s.size += info.Size()
		s.count++
		if m := info.ModTime(); m.After(s.mod) {
			s.mod = m
		}
	}
	if s.count == 0 {
		// An empty directory is a real state, distinct from a missing one: a
		// store whose last tool was removed must not stamp the same as a store
		// that was never created.
		return Stamp{names: "", count: 0, mod: time.Time{}, size: -1}
	}
	sort.Strings(names)
	s.names = strings.Join(names, "\x00")
	return s
}

// Changed reports whether was is out of date with respect to now.
func Changed(was, now Stamp) bool { return was != now }
