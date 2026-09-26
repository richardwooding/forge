// Package skills carries the Claude Code skill that teaches an agent how to
// use forge, and installs it into a repository.
//
// The skill is embedded in the binary rather than fetched, so `forge skill
// install` works on a machine with no network and cannot install a version
// that disagrees with the forge doing the installing. A skill describing
// capabilities that this build does not have would be worse than no skill.
package skills

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// files holds the skill. The all: prefix is required: without it, embed skips
// files beginning with _ or . -- and silently shipping an incomplete skill is
// exactly the sort of thing nobody notices until an agent is missing a
// reference it was told to read.
//
//go:embed all:forge
var files embed.FS

// Name is the skill's directory name, and what Claude Code lists it as.
const Name = "forge"

// relDir is where Claude Code looks for a project's skills.
const relDir = ".claude/skills"

// Result reports what an install did.
type Result struct {
	// Root is the directory the skill was written to.
	Root string
	// Written lists paths relative to Root that were created or replaced.
	Written []string
	// Skipped lists paths that already existed and were left alone.
	Skipped []string
}

// Installed reports whether anything was written.
func (r Result) Installed() bool { return len(r.Written) > 0 }

// ErrExists is returned when the skill is already present and force was not
// set. It is a distinct error because "it is already there" is usually good
// news, and should not read like a failure.
var ErrExists = errors.New("the skill is already installed")

// UserDir returns the per-user skills directory, used by Install when dir is
// empty.
func UserDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find your home directory: %w", err)
	}
	return filepath.Join(home, relDir, Name), nil
}

// ProjectDir returns the skills directory inside a repository.
func ProjectDir(repo string) string {
	return filepath.Join(repo, relDir, Name)
}

// Install writes the skill into root, which should be the directory the skill
// itself lives in -- ProjectDir or UserDir produce it.
//
// Existing files are left alone unless force is set, and the ones that were
// skipped are reported rather than passed over in silence: a stale skill that
// looks installed is worse than one that is obviously missing.
func Install(root string, force bool) (Result, error) {
	res := Result{Root: root}

	entries, err := list()
	if err != nil {
		return res, err
	}

	// Check before writing anything, so a refusal leaves nothing half-done.
	if !force {
		var present []string
		for _, rel := range entries {
			if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
				present = append(present, rel)
			}
		}
		if len(present) == len(entries) {
			res.Skipped = present
			return res, fmt.Errorf("%w at %s; pass force to replace it", ErrExists, root)
		}
	}

	for _, rel := range entries {
		dest := filepath.Join(root, rel)
		if !force {
			if _, err := os.Stat(dest); err == nil {
				res.Skipped = append(res.Skipped, rel)
				continue
			}
		}
		data, err := files.ReadFile(path(rel))
		if err != nil {
			return res, fmt.Errorf("reading the embedded skill: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return res, fmt.Errorf("creating %s: %w", filepath.Dir(dest), err)
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return res, fmt.Errorf("writing %s: %w", dest, err)
		}
		res.Written = append(res.Written, rel)
	}
	return res, nil
}

// Read returns one file of the skill, by its path relative to the skill
// directory, so `forge skill show` need not install anything to display it.
func Read(rel string) ([]byte, error) {
	if strings.Contains(rel, "..") {
		return nil, fmt.Errorf("%q is not a path inside the skill", rel)
	}
	return files.ReadFile(path(rel))
}

// Files lists the skill's contents, relative to the skill directory.
func Files() ([]string, error) { return list() }

func path(rel string) string { return Name + "/" + filepath.ToSlash(rel) }

// list walks the embedded skill. The result is sorted so that output, and the
// tests that read it, do not depend on walk order.
func list() ([]string, error) {
	var out []string
	err := fs.WalkDir(files, Name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(Name, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking the embedded skill: %w", err)
	}
	sort.Strings(out)
	return out, nil
}
