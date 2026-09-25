package cli

import "strings"

// PreScan finds the active view before the command tree is built.
//
// This exists because of an ordering problem that has no tidy solution. The set
// of subcommands depends on the view, but cobra only parses --view while
// executing the tree it was given -- by which point the tree has already been
// built with the wrong tools in it. So the flag is read from os.Args directly,
// first.
//
// The completion form matters as much as the ordinary one. A shell asks for
// completions by running `forge __complete --view dev <partial>`, and if the
// pre-scan missed that, tab completion would offer the default view's tools
// while the user was working in another -- a failure that looks like the tool
// simply not existing.
func PreScan(args []string) (view, selector string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--":
			// Everything after this belongs to the tool, not to forge.
			return view, selector
		case a == "--view":
			view = next()
		case strings.HasPrefix(a, "--view="):
			view = strings.TrimPrefix(a, "--view=")
		case a == "--labels":
			selector = next()
		case strings.HasPrefix(a, "--labels="):
			selector = strings.TrimPrefix(a, "--labels=")
		}
	}
	return view, selector
}
