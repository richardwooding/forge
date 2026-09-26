package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/richardwooding/forge/internal/ui"
	"github.com/richardwooding/forge/skills"
)

func (a *App) cmdSkill() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "skill",
		Short:   "Install the Claude Code skill that teaches an agent to use forge",
		GroupID: "manage",
		Long: "The skill tells a coding agent when to reach for an installed forge tool\n" +
			"instead of a shell pipeline, and how to write a new one. It is embedded in\n" +
			"this binary, so it always describes the forge you are actually running.",
	}

	var (
		user  bool
		force bool
	)

	install := &cobra.Command{
		Use:   "install [repo]",
		Short: "Write the skill into a repository",
		Long: "Writes .claude/skills/forge/ into the given repository, defaulting to the\n" +
			"current directory. With --user it installs once for every project instead.\n" +
			"Existing files are left alone unless --force is given.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			root, where, err := skillRoot(args, user)
			if err != nil {
				return err
			}

			res, err := skills.Install(root, force)
			if errors.Is(err, skills.ErrExists) {
				// Already installed is not a failure. Say so and stop, rather
				// than making the caller read an error to learn good news.
				fmt.Fprintf(c.OutOrStdout(), "the forge skill is already installed at %s\n", res.Root)
				fmt.Fprintln(c.OutOrStdout(), "run again with --force to replace it")
				return nil
			}
			if err != nil {
				return err
			}

			th := ui.ForWriter(c.OutOrStdout())
			fmt.Fprintf(c.OutOrStdout(), "installed the %s skill %s\n",
				th.Name.Render(skills.Name), where)
			for _, f := range res.Written {
				fmt.Fprintf(c.OutOrStdout(), "  + %s\n", filepath.Join(res.Root, f))
			}
			for _, f := range res.Skipped {
				fmt.Fprintf(c.OutOrStdout(), "  = %s (kept; --force to replace)\n", f)
			}
			fmt.Fprintln(c.OutOrStdout())
			fmt.Fprintln(c.OutOrStdout(), "start a new session in that repository for the agent to pick it up")
			return nil
		},
	}
	install.Flags().BoolVar(&user, "user", false, "install for every project, under your home directory")
	install.Flags().BoolVar(&force, "force", false, "replace files that are already there")

	show := &cobra.Command{
		Use:   "show [file]",
		Short: "Print the skill without installing it",
		Long: "Prints SKILL.md by default, or the named file, so you can read what\n" +
			"`forge skill install` would write before writing it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			name := "SKILL.md"
			if len(args) == 1 {
				name = args[0]
			}
			data, err := skills.Read(name)
			if err != nil {
				files, lerr := skills.Files()
				if lerr != nil {
					return err
				}
				return fmt.Errorf("the skill has no %q; it holds %s", name, strings.Join(files, ", "))
			}
			_, err = c.OutOrStdout().Write(data)
			return err
		},
		ValidArgsFunction: func(c *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
			files, err := skills.Files()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var out []string
			for _, f := range files {
				if strings.HasPrefix(f, prefix) {
					out = append(out, f)
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		},
	}

	cmd.AddCommand(install, show)
	return cmd
}

// skillRoot works out where to install, and a phrase describing it for the
// message afterwards.
func skillRoot(args []string, user bool) (root, where string, err error) {
	if user {
		if len(args) > 0 {
			return "", "", errors.New("--user installs under your home directory, so it takes no repository argument")
		}
		root, err = skills.UserDir()
		if err != nil {
			return "", "", err
		}
		return root, "for every project", nil
	}

	repo := "."
	if len(args) == 1 {
		repo = args[0]
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", "", fmt.Errorf("cannot resolve %q: %w", repo, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", "", fmt.Errorf("cannot install into %q: %w", repo, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("%q is not a directory", repo)
	}
	return skills.ProjectDir(abs), "in " + abs, nil
}
