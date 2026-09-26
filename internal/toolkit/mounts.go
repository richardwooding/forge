package toolkit

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/capability/rootfs"
	"github.com/richardwooding/forge/internal/wasmrt"
)

// mountsFor turns a tool's granted filesystem scopes into the mounts the
// runtime needs.
//
// Until this existed, wasmrt.Options.Mounts was declared and read but never
// assigned, so WithFSConfig was never called and the os.Root jail in
// capability/rootfs -- written, tested, and correct -- was unreachable. A tool
// could declare fs.read, be granted it, see it in `forge grant ls`, and still
// have every open fail with EBADF.
//
// Each scope is mounted at its own host path, so a tool handed
// /home/you/project reads it as /home/you/project. The alternative, mounting
// each scope at /, would silently change the meaning of every path a tool is
// given: a caller passing an absolute path from the host would be pointing at
// a different file inside the guest, and the error when it went wrong would
// name a path that exists.
func mountsFor(grants capability.Set) ([]wasmrt.Mount, error) {
	modes := map[string]rootfs.Mode{}

	// Read first, then write, so that a path granted both ends up writable:
	// fs.write implies being able to read what you wrote, and mounting the
	// same directory twice with different modes is not something wazero can
	// express anyway.
	for _, kind := range []capability.Kind{capability.FSRead, capability.FSWrite} {
		mode := rootfs.ReadOnly
		if kind == capability.FSWrite {
			mode = rootfs.ReadWrite
		}
		for _, scope := range grants.Scopes(kind) {
			path, err := mountPath(kind, scope)
			if err != nil {
				return nil, err
			}
			if mode == rootfs.ReadWrite || modes[path] != rootfs.ReadWrite {
				modes[path] = mode
			}
		}
	}

	paths := make([]string, 0, len(modes))
	for p := range modes {
		paths = append(paths, p)
	}
	// Sorted so that the mount order, and anything that depends on it, does
	// not vary between runs of the same tool with the same grants.
	sort.Strings(paths)

	mounts := make([]wasmrt.Mount, 0, len(paths))
	for _, p := range paths {
		mounts = append(mounts, wasmrt.Mount{HostPath: p, GuestPath: p, Mode: modes[p]})
	}
	return mounts, nil
}

// mountPath validates one scope as something that can actually be mounted.
//
// A scope that cannot be is refused loudly rather than skipped. Skipping is
// what produced the bug this function exists to fix: a grant that looks held
// everywhere it is displayed and does nothing where it is used.
func mountPath(kind capability.Kind, scope string) (string, error) {
	if scope == "*" {
		// Legal in a grant, and meaningless as a mount: it would be the whole
		// filesystem, which forge does not hand to a tool however the grant
		// was worded.
		return "", fmt.Errorf("%s is granted for %q, which would be the entire filesystem; "+
			"grant a directory instead (forge grant revoke, then name the path)", kind, scope)
	}
	if !filepath.IsAbs(scope) {
		return "", fmt.Errorf("%s is granted for %q, which is not an absolute path", kind, scope)
	}
	return filepath.Clean(scope), nil
}
