package build

import (
	"strings"
	"testing"
)

func TestRemapPath(t *testing.T) {
	const (
		work = "/tmp/forge-build-123"
		src  = "/home/someone/tools/hello"
	)
	tests := []struct {
		name, in, want string
	}{
		// -trimpath makes the compiler report the tool's own files relative to
		// the build directory. This is the case that matters: "./main.go"
		// resolves correctly by accident when the author is standing in the
		// tool's directory and points at nothing when they are not.
		{"relative with dot", "./main.go", src + "/main.go"},
		{"relative bare", "main.go", src + "/main.go"},
		{"relative nested", "internal/x/y.go", src + "/internal/x/y.go"},

		// Absolute paths inside the workspace.
		{"workspace file", work + "/main.go", src + "/main.go"},
		{"workspace nested", work + "/sub/a.go", src + "/sub/a.go"},
		{"workspace root", work, src},

		// A failure inside a dependency genuinely lives in the module cache;
		// rewriting it would send the author looking for a file they do not
		// have.
		{"module cache", "/home/someone/go/pkg/mod/example.com/x@v1/a.go", "/home/someone/go/pkg/mod/example.com/x@v1/a.go"},
		{"unrelated absolute", "/usr/lib/go/src/fmt/print.go", "/usr/lib/go/src/fmt/print.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := remapPath(tt.in, work, src); got != tt.want {
				t.Errorf("remapPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHintForRecognisesWasip1Failures(t *testing.T) {
	// These are the failures a competent Go developer has no reason to
	// recognise, because they are consequences of the target rather than of
	// their code.
	tests := []struct{ msg, want string }{
		{"undefined: syscall.Socket", "no sockets"},
		{"could not determine kind of name for C.foo (cgo)", "CGO_ENABLED=0"},
		{"missing go.sum entry for module", "go mod tidy"},
		{"go.mod requires go >= 1.99", "GOTOOLCHAIN=local"},
	}
	for _, tt := range tests {
		if got := hintFor(tt.msg); got == "" {
			t.Errorf("no hint for %q", tt.msg)
		} else if !strings.Contains(got, tt.want) {
			t.Errorf("hint for %q = %q, want it to mention %q", tt.msg, got, tt.want)
		}
	}
	if hintFor("undefined: someOrdinaryTypo") != "" {
		t.Error("an ordinary compile error should get no hint; noise on every error would make the useful ones invisible")
	}
}
