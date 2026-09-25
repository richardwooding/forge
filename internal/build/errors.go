package build

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Diagnostic is one compiler message, relocated to the author's own tree.
type Diagnostic struct {
	File    string
	Line    int
	Col     int
	Message string

	// Hint explains a failure mode specific to building for wasip1, which a Go
	// developer has no reason to recognise.
	Hint string
}

func (d Diagnostic) String() string {
	loc := d.File
	if d.Line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, d.Line)
		if d.Col > 0 {
			loc = fmt.Sprintf("%s:%d", loc, d.Col)
		}
	}
	if loc == "" {
		return d.Message
	}
	return loc + ": " + d.Message
}

// Error is a failed build.
type Error struct {
	Diags []Diagnostic
	Raw   string
	Cause error
}

func (e *Error) Error() string {
	if len(e.Diags) == 0 {
		if e.Raw != "" {
			return "build failed:\n" + e.Raw
		}
		return "build failed: " + e.Cause.Error()
	}
	var b strings.Builder
	b.WriteString("build failed:\n")
	for _, d := range e.Diags {
		b.WriteString("  " + d.String() + "\n")
		if d.Hint != "" {
			b.WriteString("    → " + d.Hint + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (e *Error) Unwrap() error { return e.Cause }

// diagRE matches the compiler's file:line:col: message form.
var diagRE = regexp.MustCompile(`^(.+?):(\d+)(?::(\d+))?: (.*)$`)

// buildError turns a failed go build into something the author can act on.
func (b *Builder) buildError(ctx context.Context, stdoutJSON []byte, stderr, work, srcRoot string, cause error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("the build was cancelled after %s: %w", b.cfg.Timeout, ctx.Err())
	}

	raw := collectOutput(stdoutJSON)
	if raw == "" {
		raw = stderr
	}

	e := &Error{Raw: strings.TrimSpace(raw), Cause: cause}
	for line := range strings.SplitSeq(e.Raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		d := Diagnostic{Message: line}
		if m := diagRE.FindStringSubmatch(line); m != nil {
			d.File = remapPath(m[1], work, srcRoot)
			d.Line, _ = strconv.Atoi(m[2])
			if m[3] != "" {
				d.Col, _ = strconv.Atoi(m[3])
			}
			d.Message = m[4]
		}
		d.Hint = hintFor(d.Message)
		e.Diags = append(e.Diags, d)
	}
	return e
}

// collectOutput pulls the compiler's text out of `go build -json`.
//
// The stream is newline-delimited objects with an Action of build-output or
// build-fail; anything that is not valid JSON is passed through, because a
// toolchain that failed before it started streaming still has something to say.
func collectOutput(stdout []byte) string {
	var out strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var ev struct {
			Action string `json:"Action"`
			Output string `json:"Output"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			out.Write(line)
			out.WriteByte('\n')
			continue
		}
		if ev.Action == "build-output" || ev.Action == "build-fail" {
			out.WriteString(ev.Output)
		}
	}
	return out.String()
}

// remapPath rewrites a compiler path back to the author's own tree.
//
// Two forms arrive. With -trimpath the compiler reports paths relative to the
// build directory, so an error in the tool's own source reads as "./main.go";
// without it, or for a file the toolchain names absolutely, it carries the
// throwaway workspace prefix. Both have to be relocated, and the relative case
// is the one that matters more, because "./main.go" is not obviously wrong --
// it resolves correctly by accident when the author happens to be standing in
// the tool's directory, and silently points at nothing when they are not.
//
// Paths outside the workspace are left alone: an error inside a dependency
// genuinely lives in the module cache, and rewriting it would send the author
// looking for a file they do not have.
func remapPath(path, work, srcRoot string) string {
	if path == work {
		return srcRoot
	}
	if rest, ok := strings.CutPrefix(path, work+string(filepath.Separator)); ok {
		return filepath.Join(srcRoot, rest)
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(srcRoot, strings.TrimPrefix(path, "./"))
}

// hintFor recognises the ways a build fails specifically because the target is
// wasip1, which a Go developer has no particular reason to know.
func hintFor(msg string) string {
	switch {
	case strings.Contains(msg, "syscall.Socket"),
		strings.Contains(msg, "net.Dial"),
		strings.Contains(msg, "not supported by wasip1"):
		return "wasip1 has no sockets, so a tool cannot open a network connection itself. Declare the net.http capability and use the SDK's HTTP client, which goes through forge."
	case strings.Contains(msg, "cgo"), strings.Contains(msg, "C source files not allowed"):
		return "forge builds with CGO_ENABLED=0. This dependency needs cgo, so it cannot be compiled to wasm; look for a pure-Go alternative."
	case strings.Contains(msg, "missing go.sum entry"):
		return "run `go mod tidy` in the tool's directory and commit go.sum, or let forge resolve it by building with network access."
	case strings.Contains(msg, "go.mod requires go >="),
		strings.Contains(msg, "requires go >="):
		return "forge pins GOTOOLCHAIN=local so that installing a tool cannot download a different Go. Lower the tool's go directive, or upgrade the Go on this machine."
	case strings.Contains(msg, "//go:wasmexport"):
		return "//go:wasmexport has strict rules about argument and result types. This usually means the SDK is being used in a way it does not support; check the handler signature."
	case strings.Contains(msg, "undefined: main.main"), strings.Contains(msg, "function main is undeclared"):
		return "a tool is still a main package and needs a `func main() {}`, even though it is never called in a reactor build."
	}
	return ""
}
