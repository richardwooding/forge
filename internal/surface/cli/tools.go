package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/richardwooding/forge/internal/binding"
	"github.com/richardwooding/forge/internal/store"
	"github.com/richardwooding/forge/internal/toolkit"
)

// reserved names cannot become tool subcommands, because shadowing a builtin
// would make forge's own commands unreachable once someone installed a tool
// with an unlucky name.
var reserved = map[string]bool{
	"tool": true, "run": true, "ls": true, "list": true, "info": true,
	"view": true, "label": true, "grant": true, "serve": true, "mcp": true,
	"repl": true, "doctor": true, "help": true, "completion": true,
	"version": true, "export": true, "import": true, "push": true, "pull": true,
}

// addToolCommands attaches one subcommand per in-view tool.
func (a *App) addToolCommands(root *cobra.Command, records []store.Record) {
	for _, rec := range records {
		if reserved[rec.Spec.Name] {
			// The tool is still reachable through `forge run`, so nothing is
			// lost beyond the shorthand.
			continue
		}
		root.AddCommand(a.toolCommand(rec))
	}
}

// toolCommand builds the subcommand for one tool. A tool with a single
// operation is called directly; several become sub-subcommands.
func (a *App) toolCommand(rec store.Record) *cobra.Command {
	cmd := &cobra.Command{
		Use:     rec.Spec.Name,
		Short:   rec.Spec.Summary,
		Long:    rec.Spec.Description,
		GroupID: "tools",
	}
	if op, ok := rec.Spec.DefaultOp(); ok {
		a.attachOp(cmd, rec, op.Name)
		return cmd
	}
	for _, op := range rec.Spec.Ops {
		sub := &cobra.Command{Use: op.Name, Short: op.Summary}
		a.attachOp(sub, rec, op.Name)
		cmd.AddCommand(sub)
	}
	return cmd
}

// attachOp gives a command the flags derived from an operation's input schema
// and the RunE that invokes it.
func (a *App) attachOp(cmd *cobra.Command, rec store.Record, op string) {
	b, err := a.tk.Bound(rec.Spec.Name, op)
	if err != nil {
		cmd.RunE = func(*cobra.Command, []string) error { return err }
		return
	}

	for _, f := range b.Flags.Flags {
		usage := f.Usage
		if usage == "" {
			usage = f.Name
		}
		if f.DefaultDisplay != "" {
			usage += fmt.Sprintf(" (default %s)", f.DefaultDisplay)
		}
		if f.Required {
			usage += " (required)"
		}
		switch {
		case f.Repeatable:
			cmd.Flags().Var(&schemaArray{kind: f.Kind}, f.Name, usage)
		case f.Kind == binding.FlagBool:
			cmd.Flags().Bool(f.Name, false, usage)
			// So that --loud works as well as --loud=true.
			cmd.Flags().Lookup(f.Name).NoOptDefVal = "true"
		default:
			cmd.Flags().Var(&schemaValue{kind: f.Kind}, f.Name, usage)
		}

		if len(f.Enum) > 0 {
			values := append([]string(nil), f.Enum...)
			_ = cmd.RegisterFlagCompletionFunc(f.Name,
				func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
					return values, cobra.ShellCompDirectiveNoFileComp
				})
		}
	}

	// Flags are deliberately not marked required and carry no defaults from the
	// schema. Both are done once, in Normalize, for every surface: a required
	// flag here would have cobra report a missing parameter in its own words
	// while MCP reported the same mistake in the validator's.

	cmd.Flags().String("input-json", "", "read the whole input document from a file, or - for stdin")
	cmd.Flags().StringArray("set", nil, "set a value by JSON pointer or dotted path, e.g. --set db.host=localhost")
	cmd.Flags().StringArray("set-json", nil, "set a raw JSON value, e.g. --set-json tags='[\"a\",\"b\"]'")

	if !b.Flags.Complete() {
		var sb strings.Builder
		sb.WriteString("\nSome of this tool's input cannot be expressed as flags; use --input-json or --set-json for:\n")
		for _, ne := range b.Flags.NotExpressible {
			fmt.Fprintf(&sb, "  %s — %s\n", ne.Pointer, ne.Reason)
		}
		cmd.Long += sb.String()
	}

	cmd.RunE = func(c *cobra.Command, args []string) error {
		input, err := a.buildInput(c, b)
		if err != nil {
			return err
		}
		return a.runTool(c, rec.Spec.Name, op, input)
	}
}

// buildInput layers the ways a user can supply input, lowest precedence first:
// a whole document, then pointer assignments, then explicit flags.
func (a *App) buildInput(cmd *cobra.Command, b *binding.Bound) (json.RawMessage, error) {
	doc := map[string]any{}

	if path, _ := cmd.Flags().GetString("input-json"); path != "" {
		raw, err := readDocument(cmd, path)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("--input-json: %w", err)
		}
	}

	sets, _ := cmd.Flags().GetStringArray("set")
	for _, s := range sets {
		if err := applySet(doc, s, false); err != nil {
			return nil, err
		}
	}
	setJSON, _ := cmd.Flags().GetStringArray("set-json")
	for _, s := range setJSON {
		if err := applySet(doc, s, true); err != nil {
			return nil, err
		}
	}

	// Only flags the user actually changed are collected. A flag left alone
	// must stay absent, or Normalize could not tell "--count 0" from "did not
	// say", and the schema's own default would never apply.
	values := map[string][]string{}
	var flagErr error
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if !f.Changed || isBuiltinInputFlag(f.Name) {
			return
		}
		bf, ok := b.Flags.Flag(f.Name)
		if !ok {
			return
		}
		if bf.Repeatable {
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				values[f.Name] = sv.GetSlice()
				return
			}
			flagErr = fmt.Errorf("--%s: internal error reading a repeatable flag", f.Name)
			return
		}
		values[f.Name] = []string{f.Value.String()}
	})
	if flagErr != nil {
		return nil, flagErr
	}

	if len(values) > 0 {
		flagDoc, fault := b.Flags.Document(values)
		if fault != nil {
			return nil, fault
		}
		var m map[string]any
		if err := json.Unmarshal(flagDoc, &m); err != nil {
			return nil, err
		}
		mergeInto(doc, m)
	}
	return json.Marshal(doc)
}

func isBuiltinInputFlag(name string) bool {
	switch name {
	case "input-json", "set", "set-json", "view", "labels", "output", "quiet":
		return true
	}
	return false
}

func readDocument(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	return os.ReadFile(path)
}

// applySet handles --set and --set-json.
func applySet(doc map[string]any, assignment string, raw bool) error {
	path, value, ok := strings.Cut(assignment, "=")
	if !ok {
		return fmt.Errorf("--set %q: expected path=value", assignment)
	}
	var v any = value
	if raw {
		if err := json.Unmarshal([]byte(value), &v); err != nil {
			return fmt.Errorf("--set-json %s: %w", path, err)
		}
	}
	return setPath(doc, path, v)
}

// setPath writes a value at a dotted path or JSON pointer, creating objects on
// the way.
func setPath(doc map[string]any, path string, v any) error {
	path = strings.TrimPrefix(path, "/")
	sep := "."
	if strings.Contains(path, "/") {
		sep = "/"
	}
	parts := strings.Split(path, sep)
	cur := doc
	for i, part := range parts {
		if part == "" {
			return fmt.Errorf("empty path segment in %q", path)
		}
		if i == len(parts)-1 {
			cur[part] = v
			return nil
		}
		next, ok := cur[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[part] = next
		}
		cur = next
	}
	return nil
}

// mergeInto deep-merges src over dst, so an explicit flag wins over the same
// field supplied by --input-json.
func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if sub, ok := v.(map[string]any); ok {
			if existing, ok := dst[k].(map[string]any); ok {
				mergeInto(existing, sub)
				continue
			}
		}
		dst[k] = v
	}
}

// runTool invokes the tool and writes the result.
func (a *App) runTool(cmd *cobra.Command, tool, op string, input json.RawMessage) error {
	res, err := a.tk.Invoke(cmd.Context(), toolkit.Call{
		Tool:       tool,
		Op:         op,
		Input:      input,
		OnLog:      a.logSink(cmd),
		OnProgress: a.progressSink(cmd),
	})
	if err != nil {
		return err
	}
	return a.writeResult(cmd, res)
}
