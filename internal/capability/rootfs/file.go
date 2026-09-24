package rootfs

import (
	"io"
	"io/fs"
	"os"

	expsys "github.com/tetratelabs/wazero/experimental/sys"
	wsys "github.com/tetratelabs/wazero/sys"
)

// file adapts an *os.File opened through an *os.Root to wazero's sys.File.
//
// Like FS it embeds the Unimplemented variant so that anything not handled here
// fails closed with ENOSYS. The file was already resolved inside the root when
// it was opened, so nothing in this type can escape; its job is only to enforce
// the mount's read-only mode and to translate errors.
type file struct {
	expsys.UnimplementedFile

	f  *os.File
	ro bool

	// dirents caches one Readdir traversal. WASI reads a directory in pages and
	// expects a stable ordering across calls, which a fresh ReadDir per call
	// would not give.
	dirents []expsys.Dirent
	read    bool
}

func (x *file) Close() expsys.Errno { return errno(x.f.Close()) }

func (x *file) Stat() (wsys.Stat_t, expsys.Errno) {
	fi, err := x.f.Stat()
	if err != nil {
		return wsys.Stat_t{}, expsys.UnwrapOSError(err)
	}
	return statFromFileInfo(fi), 0
}

func (x *file) IsDir() (bool, expsys.Errno) {
	fi, err := x.f.Stat()
	if err != nil {
		return false, expsys.UnwrapOSError(err)
	}
	return fi.IsDir(), 0
}

func (x *file) Read(buf []byte) (int, expsys.Errno) {
	n, err := x.f.Read(buf)
	return n, readErrno(n, err)
}

func (x *file) Pread(buf []byte, off int64) (int, expsys.Errno) {
	n, err := x.f.ReadAt(buf, off)
	return n, readErrno(n, err)
}

// Seek implements sys.File.
//
// go vet's stdmethods check flags this signature because it expects a method
// named Seek to return error, as io.Seeker does. wazero's sys.File mandates an
// Errno instead, and this type exists only to satisfy that interface, so the
// warning is wrong here and cannot be fixed by changing the signature. CI runs
// vet with -stdmethods=false and golangci-lint keeps the check everywhere else.
func (x *file) Seek(offset int64, whence int) (int64, expsys.Errno) {
	n, err := x.f.Seek(offset, whence)
	if err != nil {
		return 0, expsys.UnwrapOSError(err)
	}
	return n, 0
}

func (x *file) Write(buf []byte) (int, expsys.Errno) {
	if x.ro {
		return 0, expsys.EBADF
	}
	n, err := x.f.Write(buf)
	return n, errno(err)
}

func (x *file) Pwrite(buf []byte, off int64) (int, expsys.Errno) {
	if x.ro {
		return 0, expsys.EBADF
	}
	n, err := x.f.WriteAt(buf, off)
	return n, errno(err)
}

func (x *file) Truncate(size int64) expsys.Errno {
	if x.ro {
		return expsys.EBADF
	}
	return errno(x.f.Truncate(size))
}

func (x *file) Sync() expsys.Errno {
	if x.ro {
		return 0
	}
	return errno(x.f.Sync())
}

func (x *file) Datasync() expsys.Errno { return x.Sync() }

// Readdir implements sys.File. n <= 0 returns every remaining entry; otherwise
// it returns at most n, and an empty slice once the directory is exhausted.
func (x *file) Readdir(n int) ([]expsys.Dirent, expsys.Errno) {
	if !x.read {
		entries, err := x.f.ReadDir(-1)
		if err != nil {
			return nil, expsys.UnwrapOSError(err)
		}
		x.dirents = make([]expsys.Dirent, 0, len(entries))
		for _, e := range entries {
			x.dirents = append(x.dirents, expsys.Dirent{
				Name: e.Name(),
				Type: e.Type() & fs.ModeType,
			})
		}
		x.read = true
	}
	if n <= 0 || n > len(x.dirents) {
		n = len(x.dirents)
	}
	out := x.dirents[:n]
	x.dirents = x.dirents[n:]
	return out, 0
}

// readErrno maps a read result to an Errno. io.EOF is not an error in this ABI:
// WASI signals end of file with a zero-length read, so returning an Errno here
// would turn a normal end-of-file into a failure inside the guest.
func readErrno(n int, err error) expsys.Errno {
	switch {
	case err == nil, err == io.EOF:
		return 0
	case n > 0:
		return 0
	default:
		return expsys.UnwrapOSError(err)
	}
}
