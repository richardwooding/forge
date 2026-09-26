package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/richardwooding/forge/internal/ui"
)

func (a *App) cmdSecret() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "secret",
		Short:   "Store the credentials tools ask for",
		GroupID: "manage",
		Long: "Secrets are files under forge's config directory, readable only by you.\n" +
			"A tool reaches one through the secret capability, by name, and only if you\n" +
			"granted it that name -- it never sees the others, and never sees your\n" +
			"environment.",
	}

	set := &cobra.Command{
		Use:   "set <name> [value]",
		Short: "Store a secret",
		Long: "With no value, forge reads it from the terminal without echoing.\n" +
			"Pass - to read it from standard input, which is what a script should do:\n" +
			"a value on the command line ends up in your shell history and in ps.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			name := args[0]
			value, err := readSecretValue(c, args)
			if err != nil {
				return err
			}
			if value == "" {
				return errors.New("the secret is empty; nothing was stored")
			}
			if err := a.tk.Secrets().Put(name, value); err != nil {
				return err
			}
			th := ui.ForWriter(c.OutOrStdout())
			fmt.Fprintf(c.OutOrStdout(), "stored %s\n", th.Name.Render(name))
			fmt.Fprintf(c.OutOrStdout(), "a tool can read it once you grant it secret(%s)\n", name)
			return nil
		},
	}

	list := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "Show which secrets are stored",
		Long:    "Names only. forge never prints a secret's value.",
		RunE: func(c *cobra.Command, args []string) error {
			names, err := a.tk.Secrets().Names()
			if err != nil {
				return err
			}
			if len(names) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no secrets stored; add one with: forge secret set <name>")
				return nil
			}
			th := ui.ForWriter(c.OutOrStdout())
			for _, n := range names {
				fmt.Fprintln(c.OutOrStdout(), th.Name.Render(n))
			}
			return nil
		},
	}

	rm := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Delete a secret",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := a.tk.Secrets().Remove(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed %s\n", args[0])
			return nil
		},
		ValidArgsFunction: a.completeSecretNames,
	}

	cmd.AddCommand(set, list, rm)
	return cmd
}

// readSecretValue gets the value from the argument, from stdin, or from the
// terminal, in that order.
func readSecretValue(c *cobra.Command, args []string) (string, error) {
	if len(args) == 2 && args[1] != "-" {
		return args[1], nil
	}
	if len(args) == 2 && args[1] == "-" {
		data, err := io.ReadAll(c.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("reading the secret from standard input: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}

	// No value given. Prompt if there is someone to ask; otherwise read stdin,
	// so `forge secret set token < file` works without the explicit dash.
	//
	// term.IsTerminal rather than the package's isTerminal: that one also
	// honours NO_COLOR, which has nothing to do with whether there is a person
	// here to type a password.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		data, err := io.ReadAll(c.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("reading the secret from standard input: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}

	fmt.Fprintf(c.ErrOrStderr(), "value for %s: ", args[0])
	value, err := readHidden(os.Stdin)
	fmt.Fprintln(c.ErrOrStderr())
	if err != nil {
		return "", err
	}
	return value, nil
}

// readHidden reads one line without echoing it.
//
// The prompt goes to stderr and the value never reaches stdout, so a secret
// cannot end up in a redirect the user forgot about.
func readHidden(f *os.File) (string, error) {
	data, err := term.ReadPassword(int(f.Fd()))
	if err != nil {
		// No terminal control available: read it plainly rather than refuse.
		// Echoing is bad; refusing to store the secret at all is worse, and
		// the user chose this terminal.
		line, rerr := bufio.NewReader(f).ReadString('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return "", rerr
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

func (a *App) completeSecretNames(c *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
	names, err := a.tk.Secrets().Names()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
