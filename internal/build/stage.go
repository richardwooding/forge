package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// maxSourceBytes caps a staged source tree. A tool is a small program; anything
// larger is a mistake -- most often a directory that happens to contain a
// dataset or a vendored checkout the author did not mean to install.
const maxSourceBytes = 64 << 20

// stage copies the source into the throwaway workspace.
func (b *Builder) stage(src string, info os.FileInfo, work string) error {
	if !info.IsDir() {
		if filepath.Ext(src) != ".go" {
			return fmt.Errorf("%s is not a Go file or a directory", src)
		}
		body, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(work, "main.go"), body, 0o644)
	}
	return copyTree(src, work)
}

// skipDir names directories never worth copying into a build.
func skipDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules", ".forge", "dist", ".idea", ".vscode":
		return true
	}
	return false
}

func copyTree(src, dst string) error {
	var total int64
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		// Symlinks are not followed. A link pointing outside the source tree
		// would drag in whatever it names, and a link pointing at a device or
		// socket would hang the copy.
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += fi.Size()
		if total > maxSourceBytes {
			return fmt.Errorf("source tree is larger than %d MiB; is %s the tool's directory, or something bigger?", maxSourceBytes>>20, src)
		}
		return copyFile(path, filepath.Join(dst, rel))
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ensureModule gives the staged copy a go.mod, and applies the SDK replace when
// one is configured.
func (b *Builder) ensureModule(ctx context.Context, goBin, work string) error {
	modPath := filepath.Join(work, "go.mod")
	_, err := os.Stat(modPath)
	switch {
	case err == nil:
		// The author's own module. Leave its requirements alone.
	case os.IsNotExist(err):
		if err := b.run(ctx, goBin, work, "mod", "init", "forge.local/tool"); err != nil {
			return err
		}
	default:
		return err
	}

	if b.cfg.SDKReplace != "" {
		abs, err := filepath.Abs(b.cfg.SDKReplace)
		if err != nil {
			return err
		}
		// Written into the throwaway copy only. The author's own go.mod is
		// never touched, so a replace cannot leak into what they commit.
		if err := b.run(ctx, goBin, work, "mod", "edit",
			"-replace=github.com/richardwooding/forge/sdk="+abs); err != nil {
			return err
		}
	}

	// tidy resolves the SDK and anything else the source imports. It needs the
	// network unless the module cache already has them, which is why an offline
	// build says so explicitly rather than failing with a checksum error.
	if err := b.run(ctx, goBin, work, "mod", "tidy"); err != nil {
		if b.cfg.Proxy == "off" {
			return fmt.Errorf("%w\n\nthe build is offline, so dependencies must already be in forge's module cache; run once with network, or add a go.mod and go.sum to the tool", err)
		}
		return err
	}
	return nil
}

// run executes a go subcommand in the staged workspace.
func (b *Builder) run(ctx context.Context, goBin, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, goBin, args...)
	cmd.Dir = dir
	cmd.Env = buildEnv(b.cfg)
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
