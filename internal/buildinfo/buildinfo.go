// Package buildinfo reports which forge this is.
//
// The values are set by the linker at release time and fall back to Go's own
// embedded build information otherwise, so a binary built with `go install`
// or `go build` still identifies itself rather than claiming to be "dev".
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set by the linker: -X .../internal/buildinfo.version=... and so on.
var (
	version = ""
	commit  = ""
	date    = ""
)

// Version is the release version, or a best effort for an unreleased build.
func Version() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// Commit is the revision this was built from.
func Commit() string {
	if commit != "" {
		return commit
	}
	return vcs("vcs.revision")
}

// Date is when it was built.
func Date() string {
	if date != "" {
		return date
	}
	return vcs("vcs.time")
}

// Modified reports whether the working tree had uncommitted changes.
//
// Worth surfacing: a bug report from a modified build is a report about
// something no tag describes, and knowing that early saves chasing it.
func Modified() bool { return vcs("vcs.modified") == "true" }

func vcs(key string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// String is the one-line summary `forge version` prints.
func String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "forge %s", Version())
	if c := Commit(); c != "" {
		short := c
		if len(short) > 12 {
			short = short[:12]
		}
		fmt.Fprintf(&b, " (%s", short)
		if Modified() {
			b.WriteString(", modified")
		}
		b.WriteByte(')')
	}
	fmt.Fprintf(&b, "\n  %s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if d := Date(); d != "" {
		fmt.Fprintf(&b, "\n  built %s", d)
	}
	return b.String()
}
