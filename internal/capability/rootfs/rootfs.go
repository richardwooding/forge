// Package rootfs is the filesystem jail forge mounts into a wasm guest.
//
// It exists because wazero's own directory mounts are not jails. wazero's
// fsconfig.go documents, verbatim, that WithDirMount gives the guest "full
// access to this directory including escaping it via relative path lookups like
// `../../`", and that WithReadOnlyDirMount still permits the same escape. Those
// are fine for trusted modules and unusable for tool code a stranger wrote.
//
// Instead every path operation goes through an *os.Root (Go 1.24+), which
// resolves names with openat and refuses any component that would leave the
// root — including symlinks that point outside it, whether by relative or
// absolute target.
//
// The type embeds sys.UnimplementedFS, so any method this package has not
// deliberately implemented fails closed with ENOSYS rather than falling back to
// something ambient. A Go wasip1 guest in practice reaches only OpenFile and
// Lstat for reads, so the surface that must be right is small; the rest is
// implemented for completeness and for guests written in other languages.
package rootfs

import (
	"io/fs"
	"os"
	"time"

	expsys "github.com/tetratelabs/wazero/experimental/sys"
	wsys "github.com/tetratelabs/wazero/sys"
)

// Mode selects whether the mount permits modification.
type Mode int

const (
	ReadOnly Mode = iota
	ReadWrite
)

func (m Mode) String() string {
	if m == ReadWrite {
		return "rw"
	}
	return "ro"
}

// FS adapts an *os.Root to wazero's experimental sys.FS.
//
// It does not own the *os.Root: whoever opened it closes it, normally when the
// invocation ends.
type FS struct {
	expsys.UnimplementedFS

	root *os.Root
	mode Mode
}

// New returns an FS serving root at the given mode.
func New(root *os.Root, mode Mode) *FS { return &FS{root: root, mode: mode} }

func (f *FS) String() string {
	if !f.usable() {
		return "rootfs:<invalid>"
	}
	return "rootfs:" + f.root.Name() + ":" + f.mode.String()
}

// readOnly reports whether a mutating operation should be refused outright.
func (f *FS) readOnly() bool { return f.mode != ReadWrite }

// usable reports whether this FS was built by New. A zero FS has no root, and a
// security boundary must fail closed rather than panic when it is misassembled:
// a nil dereference inside a host function becomes a wasm trap that tells the
// guest nothing and leaves the host log to explain it.
func (f *FS) usable() bool { return f != nil && f.root != nil }

// writeFlags are the open flags that make an open a mutation.
const writeFlags = expsys.O_WRONLY | expsys.O_RDWR | expsys.O_CREAT | expsys.O_TRUNC | expsys.O_APPEND | expsys.O_EXCL

// OpenFile implements sys.FS.
func (f *FS) OpenFile(path string, flag expsys.Oflag, perm fs.FileMode) (expsys.File, expsys.Errno) {
	if !f.usable() {
		return nil, expsys.EBADF
	}
	if f.readOnly() && flag&writeFlags != 0 {
		return nil, expsys.EROFS
	}
	osf, err := f.root.OpenFile(path, toOSFlag(flag), perm)
	if err != nil {
		return nil, expsys.UnwrapOSError(err)
	}
	return &file{f: osf, ro: f.readOnly()}, 0
}

// Stat implements sys.FS, following symlinks.
func (f *FS) Stat(path string) (wsys.Stat_t, expsys.Errno) {
	if !f.usable() {
		return wsys.Stat_t{}, expsys.EBADF
	}
	fi, err := f.root.Stat(path)
	if err != nil {
		return wsys.Stat_t{}, expsys.UnwrapOSError(err)
	}
	return statFromFileInfo(fi), 0
}

// Lstat implements sys.FS without following the final symlink.
func (f *FS) Lstat(path string) (wsys.Stat_t, expsys.Errno) {
	if !f.usable() {
		return wsys.Stat_t{}, expsys.EBADF
	}
	fi, err := f.root.Lstat(path)
	if err != nil {
		return wsys.Stat_t{}, expsys.UnwrapOSError(err)
	}
	return statFromFileInfo(fi), 0
}

// Readlink implements sys.FS. It is permitted on a read-only mount: reading a
// link's target reveals nothing the guest could not learn by trying to open it,
// and the target is still resolved through the root on any subsequent open.
func (f *FS) Readlink(path string) (string, expsys.Errno) {
	if !f.usable() {
		return "", expsys.EBADF
	}
	s, err := f.root.Readlink(path)
	if err != nil {
		return "", expsys.UnwrapOSError(err)
	}
	return s, 0
}

// Mkdir implements sys.FS.
func (f *FS) Mkdir(path string, perm fs.FileMode) expsys.Errno {
	if !f.usable() {
		return expsys.EBADF
	}
	if f.readOnly() {
		return expsys.EROFS
	}
	return errno(f.root.Mkdir(path, perm))
}

// Chmod implements sys.FS.
func (f *FS) Chmod(path string, perm fs.FileMode) expsys.Errno {
	if !f.usable() {
		return expsys.EBADF
	}
	if f.readOnly() {
		return expsys.EROFS
	}
	return errno(f.root.Chmod(path, perm))
}

// Rename implements sys.FS. Both names resolve inside the same root.
func (f *FS) Rename(from, to string) expsys.Errno {
	if !f.usable() {
		return expsys.EBADF
	}
	if f.readOnly() {
		return expsys.EROFS
	}
	return errno(f.root.Rename(from, to))
}

// Rmdir implements sys.FS. It refuses non-directories so that a guest cannot
// use it to delete a regular file that Unlink would have been checked for.
func (f *FS) Rmdir(path string) expsys.Errno {
	if !f.usable() {
		return expsys.EBADF
	}
	if f.readOnly() {
		return expsys.EROFS
	}
	fi, err := f.root.Lstat(path)
	if err != nil {
		return expsys.UnwrapOSError(err)
	}
	if !fi.IsDir() {
		return expsys.ENOTDIR
	}
	return errno(f.root.Remove(path))
}

// Unlink implements sys.FS. It refuses directories, mirroring POSIX.
func (f *FS) Unlink(path string) expsys.Errno {
	if !f.usable() {
		return expsys.EBADF
	}
	if f.readOnly() {
		return expsys.EROFS
	}
	fi, err := f.root.Lstat(path)
	if err != nil {
		return expsys.UnwrapOSError(err)
	}
	if fi.IsDir() {
		return expsys.EISDIR
	}
	return errno(f.root.Remove(path))
}

// Link implements sys.FS.
func (f *FS) Link(oldPath, newPath string) expsys.Errno {
	if !f.usable() {
		return expsys.EBADF
	}
	if f.readOnly() {
		return expsys.EROFS
	}
	return errno(f.root.Link(oldPath, newPath))
}

// Symlink implements sys.FS.
//
// oldPath is the link's target and is NOT resolved here — that is how symlinks
// work, and it is safe because every later traversal of the link goes back
// through the root, which refuses a target that escapes. A guest can therefore
// create a link pointing at /etc/passwd and will never be able to read it.
func (f *FS) Symlink(oldPath, linkName string) expsys.Errno {
	if !f.usable() {
		return expsys.EBADF
	}
	if f.readOnly() {
		return expsys.EROFS
	}
	return errno(f.root.Symlink(oldPath, linkName))
}

// Utimens implements sys.FS.
func (f *FS) Utimens(path string, atim, mtim int64) expsys.Errno {
	if !f.usable() {
		return expsys.EBADF
	}
	if f.readOnly() {
		return expsys.EROFS
	}
	at, mt := time.Unix(0, atim), time.Unix(0, mtim)
	if atim == expsys.UTIME_OMIT || mtim == expsys.UTIME_OMIT {
		fi, err := f.root.Lstat(path)
		if err != nil {
			return expsys.UnwrapOSError(err)
		}
		if atim == expsys.UTIME_OMIT {
			at = fi.ModTime()
		}
		if mtim == expsys.UTIME_OMIT {
			mt = fi.ModTime()
		}
	}
	return errno(f.root.Chtimes(path, at, mt))
}

func errno(err error) expsys.Errno {
	if err == nil {
		return 0
	}
	return expsys.UnwrapOSError(err)
}

// toOSFlag converts wazero's Oflag bits to the os package's flags. wazero
// defines its own values, so this cannot be a cast.
func toOSFlag(flag expsys.Oflag) int {
	var out int
	switch {
	case flag&expsys.O_WRONLY != 0:
		out = os.O_WRONLY
	case flag&expsys.O_RDWR != 0:
		out = os.O_RDWR
	default:
		out = os.O_RDONLY
	}
	for _, m := range []struct {
		from expsys.Oflag
		to   int
	}{
		{expsys.O_APPEND, os.O_APPEND},
		{expsys.O_CREAT, os.O_CREATE},
		{expsys.O_EXCL, os.O_EXCL},
		{expsys.O_SYNC, os.O_SYNC},
		{expsys.O_TRUNC, os.O_TRUNC},
	} {
		if flag&m.from != 0 {
			out |= m.to
		}
	}
	return out
}

func statFromFileInfo(fi fs.FileInfo) wsys.Stat_t {
	mt := fi.ModTime().UnixNano()
	return wsys.Stat_t{
		Mode:  fi.Mode(),
		Size:  fi.Size(),
		Nlink: 1,
		Atim:  mt,
		Mtim:  mt,
		Ctim:  mt,
	}
}
