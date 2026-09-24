package rootfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	expsys "github.com/tetratelabs/wazero/experimental/sys"
)

// tree builds a fixture with a jail directory and secrets outside it:
//
//	<tmp>/secret.txt          the thing that must stay unreachable
//	<tmp>/jail/hello.txt      an ordinary file inside
//	<tmp>/jail/sub/           a subdirectory
//	<tmp>/jail/rel-escape  -> ../secret.txt
//	<tmp>/jail/abs-escape  -> /etc/passwd
//	<tmp>/jail/sub/up      -> ../../secret.txt
func tree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mk := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("secret.txt", "TOP SECRET")
	mk("jail/hello.txt", "hello")
	if err := os.MkdirAll(filepath.Join(dir, "jail/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, ln := range []struct{ target, name string }{
		{"../secret.txt", "jail/rel-escape"},
		{"/etc/passwd", "jail/abs-escape"},
		{"../../secret.txt", "jail/sub/up"},
	} {
		if err := os.Symlink(ln.target, filepath.Join(dir, ln.name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func open(t *testing.T, dir string, mode Mode) *FS {
	t.Helper()
	root, err := os.OpenRoot(filepath.Join(dir, "jail"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return New(root, mode)
}

// TestEscapesAreRefused is the test this package exists for. Every one of these
// paths, if it succeeded, would hand a tool a file the user never granted.
func TestEscapesAreRefused(t *testing.T) {
	dir := tree(t)
	fsys := open(t, dir, ReadWrite)

	escapes := []struct {
		name, path string
	}{
		{"parent traversal", "../secret.txt"},
		{"deep traversal", "../../../etc/passwd"},
		{"absolute path", "/etc/passwd"},
		{"traversal through a real subdir", "sub/../../secret.txt"},
		{"relative symlink out", "rel-escape"},
		{"absolute symlink out", "abs-escape"},
		{"symlink out from a subdir", "sub/up"},
		{"traversal hidden mid-path", "sub/./../../secret.txt"},
	}
	for _, e := range escapes {
		t.Run(e.name, func(t *testing.T) {
			f, errno := fsys.OpenFile(e.path, expsys.O_RDONLY, 0)
			if errno == 0 {
				defer f.Close()
				buf := make([]byte, 64)
				n, _ := f.Read(buf)
				t.Fatalf("ESCAPED: opened %q and read %q", e.path, buf[:n])
			}
			// Stat must not leak existence either.
			if _, errno := fsys.Stat(e.path); errno == 0 {
				t.Fatalf("ESCAPED: stat succeeded for %q", e.path)
			}
		})
	}
}

// TestEscapeBySymlinkCreatedByTheGuest covers the case where the tool itself
// plants the link at runtime. Creating it is allowed; following it is not.
func TestEscapeBySymlinkCreatedByTheGuest(t *testing.T) {
	dir := tree(t)
	fsys := open(t, dir, ReadWrite)

	if errno := fsys.Symlink("../secret.txt", "planted"); errno != 0 {
		t.Fatalf("Symlink: %v", errno)
	}
	if _, errno := fsys.OpenFile("planted", expsys.O_RDONLY, 0); errno == 0 {
		t.Fatal("ESCAPED: followed a symlink the guest planted at runtime")
	}
}

func TestOrdinaryAccessInsideTheJailWorks(t *testing.T) {
	dir := tree(t)
	fsys := open(t, dir, ReadWrite)

	f, errno := fsys.OpenFile("hello.txt", expsys.O_RDONLY, 0)
	if errno != 0 {
		t.Fatalf("OpenFile: %v", errno)
	}
	defer f.Close()
	buf := make([]byte, 16)
	n, errno := f.Read(buf)
	if errno != 0 {
		t.Fatalf("Read: %v", errno)
	}
	if got := string(buf[:n]); got != "hello" {
		t.Errorf("Read = %q, want %q", got, "hello")
	}
}

func TestReadAtEOFIsNotAnError(t *testing.T) {
	// WASI signals end of file with a zero-length read. Returning an Errno for
	// io.EOF would surface a normal end of file as a failure inside the guest.
	dir := tree(t)
	fsys := open(t, dir, ReadOnly)

	f, errno := fsys.OpenFile("hello.txt", expsys.O_RDONLY, 0)
	if errno != 0 {
		t.Fatalf("OpenFile: %v", errno)
	}
	defer f.Close()
	if _, errno := f.Read(make([]byte, 64)); errno != 0 {
		t.Fatalf("first Read: %v", errno)
	}
	n, errno := f.Read(make([]byte, 64))
	if errno != 0 || n != 0 {
		t.Errorf("Read at EOF = (%d, %v), want (0, 0)", n, errno)
	}
}

func TestReadOnlyMountRefusesEveryMutation(t *testing.T) {
	dir := tree(t)
	fsys := open(t, dir, ReadOnly)

	mutations := map[string]expsys.Errno{
		"OpenFile O_WRONLY": func() expsys.Errno { _, e := fsys.OpenFile("hello.txt", expsys.O_WRONLY, 0); return e }(),
		"OpenFile O_CREAT":  func() expsys.Errno { _, e := fsys.OpenFile("new.txt", expsys.O_CREAT|expsys.O_WRONLY, 0o644); return e }(),
		"OpenFile O_TRUNC":  func() expsys.Errno { _, e := fsys.OpenFile("hello.txt", expsys.O_TRUNC, 0); return e }(),
		"Mkdir":             fsys.Mkdir("d", 0o755),
		"Chmod":             fsys.Chmod("hello.txt", 0o600),
		"Rename":            fsys.Rename("hello.txt", "other.txt"),
		"Unlink":            fsys.Unlink("hello.txt"),
		"Rmdir":             fsys.Rmdir("sub"),
		"Symlink":           fsys.Symlink("hello.txt", "link"),
		"Link":              fsys.Link("hello.txt", "hard"),
		"Utimens":           fsys.Utimens("hello.txt", 0, 0),
	}
	for name, errno := range mutations {
		if errno != expsys.EROFS {
			t.Errorf("%s on a read-only mount = %v, want EROFS", name, errno)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "jail", "hello.txt")); err != nil || string(b) != "hello" {
		t.Errorf("read-only mount modified the file: %q, %v", b, err)
	}
}

func TestReadOnlyFileHandleRefusesWrites(t *testing.T) {
	// Defence in depth: even if an open slipped through, the handle itself
	// must refuse to write.
	dir := tree(t)
	fsys := open(t, dir, ReadOnly)

	f, errno := fsys.OpenFile("hello.txt", expsys.O_RDONLY, 0)
	if errno != 0 {
		t.Fatalf("OpenFile: %v", errno)
	}
	defer f.Close()
	if _, errno := f.Write([]byte("x")); errno != expsys.EBADF {
		t.Errorf("Write = %v, want EBADF", errno)
	}
	if errno := f.Truncate(0); errno != expsys.EBADF {
		t.Errorf("Truncate = %v, want EBADF", errno)
	}
}

func TestReadWriteMountAllowsWritesInsideTheJail(t *testing.T) {
	dir := tree(t)
	fsys := open(t, dir, ReadWrite)

	f, errno := fsys.OpenFile("out.txt", expsys.O_CREAT|expsys.O_WRONLY, 0o644)
	if errno != 0 {
		t.Fatalf("OpenFile: %v", errno)
	}
	if _, errno := f.Write([]byte("written")); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}
	if errno := f.Close(); errno != 0 {
		t.Fatalf("Close: %v", errno)
	}
	b, err := os.ReadFile(filepath.Join(dir, "jail", "out.txt"))
	if err != nil || string(b) != "written" {
		t.Errorf("file on disk = %q, %v; want %q", b, err, "written")
	}
}

func TestUnlinkAndRmdirRefuseTheWrongFileType(t *testing.T) {
	dir := tree(t)
	fsys := open(t, dir, ReadWrite)

	if errno := fsys.Unlink("sub"); errno != expsys.EISDIR {
		t.Errorf("Unlink(dir) = %v, want EISDIR", errno)
	}
	if errno := fsys.Rmdir("hello.txt"); errno != expsys.ENOTDIR {
		t.Errorf("Rmdir(file) = %v, want ENOTDIR", errno)
	}
}

func TestReaddirPagesAndTerminates(t *testing.T) {
	dir := tree(t)
	fsys := open(t, dir, ReadOnly)

	f, errno := fsys.OpenFile(".", expsys.O_RDONLY, 0)
	if errno != 0 {
		t.Fatalf("OpenFile(.): %v", errno)
	}
	defer f.Close()

	var names []string
	for {
		ents, errno := f.Readdir(2)
		if errno != 0 {
			t.Fatalf("Readdir: %v", errno)
		}
		if len(ents) == 0 {
			break
		}
		if len(ents) > 2 {
			t.Fatalf("Readdir(2) returned %d entries", len(ents))
		}
		for _, e := range ents {
			names = append(names, e.Name)
		}
	}
	// hello.txt, sub, rel-escape, abs-escape — the links are visible; following
	// them is what is refused.
	if len(names) != 4 {
		t.Errorf("Readdir saw %v, want 4 entries", names)
	}
	if !strings.Contains(strings.Join(names, ","), "hello.txt") {
		t.Errorf("Readdir missed hello.txt: %v", names)
	}
}

func TestZeroFSFailsClosedRatherThanPanicking(t *testing.T) {
	// A zero FS has no root. Inside a host function a nil dereference becomes a
	// wasm trap that tells the guest nothing, so every entry point must refuse
	// with an Errno instead.
	var fsys FS

	if _, errno := fsys.OpenFile("x", expsys.O_RDONLY, 0); errno != expsys.EBADF {
		t.Errorf("OpenFile on a zero FS = %v, want EBADF", errno)
	}
	if _, errno := fsys.Stat("x"); errno != expsys.EBADF {
		t.Errorf("Stat = %v, want EBADF", errno)
	}
	if _, errno := fsys.Lstat("x"); errno != expsys.EBADF {
		t.Errorf("Lstat = %v, want EBADF", errno)
	}
	if _, errno := fsys.Readlink("x"); errno != expsys.EBADF {
		t.Errorf("Readlink = %v, want EBADF", errno)
	}
	if errno := fsys.Mkdir("x", 0o755); errno != expsys.EBADF {
		t.Errorf("Mkdir = %v, want EBADF", errno)
	}
	if got := fsys.String(); got != "rootfs:<invalid>" {
		t.Errorf("String = %q", got)
	}
}
