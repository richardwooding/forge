// Command forge is a multitool that builds itself.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/surface/cli"
	"github.com/richardwooding/forge/internal/toolkit"
)

func main() {
	os.Exit(run())
}

func run() int {
	// Ctrl-C cancels the context, which closes any running wasm module: a tool
	// looping forever is stopped by the runtime, not by asking it politely.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The command tree depends on the view, and cobra parses flags only while
	// executing the tree it was given, so the view has to be read from the
	// arguments first.
	viewName, selectorExpr := cli.PreScan(os.Args)

	tk, err := toolkit.New(ctx, toolkit.Config{
		SDKReplace: os.Getenv("FORGE_SDK_DIR"),
		Offline:    os.Getenv("FORGE_OFFLINE") != "",
		// On a terminal this asks; anywhere else it denies, because a prompt
		// written to a pipe is a hang rather than a question.
		Prompter: cli.TerminalPrompter{},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "forge:", err)
		return 2
	}
	defer tk.Close(context.Background())

	selector, resolvedView, err := tk.Views().Resolve(viewName, selectorExpr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forge:", err)
		return 2
	}
	if selector == nil {
		selector = labels.All
	}

	root, err := cli.New(cli.Options{
		Toolkit:  tk,
		Selector: selector,
		ViewName: resolvedView,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "forge:", err)
		return 2
	}

	if err := root.ExecuteContext(ctx); err != nil {
		cli.Render(os.Stderr, err)
		return cli.ExitCode(err)
	}
	return 0
}
