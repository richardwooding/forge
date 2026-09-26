// Package web serves forge's optional browser GUI: a list of the tools in a
// view, a form generated from each operation's schema, and the result of
// running it.
//
// It is a client of the REST surface rather than a sixth surface of its own.
// The page posts to the same /v1/.../invoke endpoint everything else uses, so
// input is validated by the same binding.Normalize and cannot drift from what
// the CLI, MCP, REST and gRPC accept. The only thing the GUI knows how to do
// that they do not is draw a form.
//
// It is off unless `forge serve --gui` asks for it. The default page at / is
// still the server-rendered, script-free index in internal/surface/rest, and
// stays that way.
package web

import (
	"embed"
	"errors"
	"io/fs"
)

// dist holds the built front end.
//
// The all: prefix matters: without it, embed skips files whose names begin
// with a dot or an underscore, and bundlers emit both. Shipping a bundle with
// its chunks silently missing is the kind of fault that only shows up in a
// browser, on someone else's machine.
//
//go:embed all:dist
var dist embed.FS

// ErrNotBuilt reports that the front end has not been built.
//
// dist/.gitkeep is committed so that //go:embed compiles in a fresh checkout,
// which means an unbuilt tree produces a binary that is missing only the
// assets. Saying so plainly beats serving a blank page.
var ErrNotBuilt = errors.New("the GUI has not been built")

// Assets returns the built front end, or ErrNotBuilt when the tree has only
// the placeholder in it.
func Assets() (fs.FS, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, ErrNotBuilt
	}
	return sub, nil
}

// Built reports whether the assets are present, for a caller that wants to
// decide rather than handle an error.
func Built() bool {
	_, err := Assets()
	return err == nil
}
