package cli

import "testing"

func TestPreScan(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		view, labels string
	}{
		{"nothing", []string{"forge", "ls"}, "", ""},
		{"separate value", []string{"forge", "--view", "dev", "ls"}, "dev", ""},
		{"equals form", []string{"forge", "--view=dev", "ls"}, "dev", ""},
		{"labels", []string{"forge", "--labels", "git && !slow", "ls"}, "", "git && !slow"},
		{"both", []string{"forge", "--view=a", "--labels=b", "ls"}, "a", "b"},
		{"after the subcommand", []string{"forge", "ls", "--view", "dev"}, "dev", ""},

		// The shell asks for completions by running the binary. Missing this
		// would offer the default view's tools while the user works in another,
		// which looks like the tool not existing.
		{"completion form", []string{"forge", "__complete", "--view", "dev", "git"}, "dev", ""},

		// Anything after -- belongs to the tool. A tool with its own --view
		// flag must not have it stolen.
		{"after a double dash", []string{"forge", "mytool", "--", "--view", "theirs"}, "", ""},
		{"before and after", []string{"forge", "--view", "ours", "mytool", "--", "--view", "theirs"}, "ours", ""},

		{"missing value", []string{"forge", "--view"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view, labels := PreScan(tt.args)
			if view != tt.view || labels != tt.labels {
				t.Errorf("PreScan(%v) = (%q, %q), want (%q, %q)", tt.args, view, labels, tt.view, tt.labels)
			}
		})
	}
}
