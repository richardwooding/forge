package skills_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richardwooding/forge/skills"
)

// TestSkillIsWellFormed checks the things Claude Code needs in order to load
// the skill at all. A skill with a malformed header is not a broken feature,
// it is an invisible one: nothing reports an error, it simply never triggers.
func TestSkillIsWellFormed(t *testing.T) {
	data, err := skills.Read("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)

	if !strings.HasPrefix(body, "---\n") {
		t.Fatal("SKILL.md does not start with YAML frontmatter")
	}
	end := strings.Index(body[4:], "\n---\n")
	if end < 0 {
		t.Fatal("the frontmatter is not closed")
	}
	front := body[4 : end+4]

	for _, field := range []string{"name:", "description:"} {
		if !strings.Contains(front, field) {
			t.Errorf("the frontmatter has no %s", field)
		}
	}
	if !strings.Contains(front, "name: "+skills.Name) {
		t.Errorf("the frontmatter name should be %q so the directory and the skill agree", skills.Name)
	}

	// The house rule is 500 lines, target 200. Past that it should be split
	// into references rather than left to grow.
	if n := strings.Count(body, "\n"); n > 500 {
		t.Errorf("SKILL.md is %d lines; the limit is 500", n)
	}
}

// TestReferencesAreReachable guards the one structural rule that breaks
// silently: SKILL.md links to reference files, and a link to a file that is
// not shipped sends an agent looking for something that is not there.
func TestReferencesAreReachable(t *testing.T) {
	data, err := skills.Read("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	files, err := skills.Files()
	if err != nil {
		t.Fatal(err)
	}
	shipped := map[string]bool{}
	for _, f := range files {
		shipped[f] = true
	}

	body := string(data)
	var linked int
	for part := range strings.SplitSeq(body, "](") {
		before, _, ok := strings.Cut(part, ")")
		if !ok {
			continue
		}
		link := before
		if !strings.HasPrefix(link, "references/") {
			continue
		}
		linked++
		if !shipped[link] {
			t.Errorf("SKILL.md links to %q, which is not shipped with the skill", link)
		}
	}
	if linked == 0 {
		t.Error("no reference links found; the split into references is not wired up")
	}

	// And the other direction: a reference nobody links to will never be read.
	for _, f := range files {
		if !strings.HasPrefix(f, "references/") {
			continue
		}
		if !strings.Contains(body, "("+f+")") {
			t.Errorf("%s is shipped but SKILL.md never links to it", f)
		}
	}
}

func TestInstallWritesTheWholeSkill(t *testing.T) {
	repo := t.TempDir()
	root := skills.ProjectDir(repo)

	res, err := skills.Install(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Installed() {
		t.Fatal("nothing was written")
	}

	want, err := skills.Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != len(want) {
		t.Errorf("wrote %d files, want %d", len(res.Written), len(want))
	}
	for _, f := range want {
		p := filepath.Join(root, f)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was not written: %v", f, err)
		}
	}

	// The path is the one Claude Code looks in, which is the whole point.
	if got := filepath.Join(repo, ".claude", "skills", "forge", "SKILL.md"); got != filepath.Join(root, "SKILL.md") {
		t.Errorf("installed to %s, want %s", filepath.Join(root, "SKILL.md"), got)
	}
}

// TestInstallDoesNotClobber: someone may have edited the skill for their repo,
// and a second install should not quietly throw that away.
func TestInstallDoesNotClobber(t *testing.T) {
	root := skills.ProjectDir(t.TempDir())
	if _, err := skills.Install(root, false); err != nil {
		t.Fatal(err)
	}

	edited := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(edited, []byte("---\nname: forge\n---\nlocal edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := skills.Install(root, false)
	if !errors.Is(err, skills.ErrExists) {
		t.Fatalf("a second install should report ErrExists, got %v", err)
	}
	data, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "local edit") {
		t.Error("the local edit was overwritten without force")
	}

	// force replaces it.
	if _, err := skills.Install(root, true); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "local edit") {
		t.Error("force did not replace the file")
	}
}

// TestInstallFillsGaps covers the half-installed case: a reference deleted by
// hand should come back without needing force, since there is nothing there to
// preserve.
func TestInstallFillsGaps(t *testing.T) {
	root := skills.ProjectDir(t.TempDir())
	if _, err := skills.Install(root, false); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "references", "capabilities.md")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	res, err := skills.Install(root, false)
	if err != nil {
		t.Fatalf("filling a gap should not need force: %v", err)
	}
	if len(res.Written) != 1 {
		t.Errorf("wrote %v, want just the missing reference", res.Written)
	}
	if len(res.Skipped) == 0 {
		t.Error("the files that were kept should be reported, not passed over silently")
	}
	if _, err := os.Stat(gone); err != nil {
		t.Errorf("the missing reference was not restored: %v", err)
	}
}

func TestReadRefusesEscape(t *testing.T) {
	for _, bad := range []string{"../skills.go", "../../go.mod"} {
		if _, err := skills.Read(bad); err == nil {
			t.Errorf("Read(%q) was allowed", bad)
		}
	}
}
