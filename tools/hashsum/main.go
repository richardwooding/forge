// Command hashsum computes and verifies message digests, over text or over a
// file on disk.
//
// The file half is the point. An earlier version of this tool took only a
// string, which confined it to data already in the caller's context -- and data
// already in the caller's context is precisely the case where a tool is not
// worth calling, because whoever has the bytes can hash them another way. A
// tool earns a call when it reaches something the caller cannot. So this one
// reads files, and the size that makes `sha256sum` the only realistic option is
// exactly the size it is for.
//
// verify against a checksums file is the operation that does more than the
// shell. `grep name checksums.txt | sha256sum -c -` is three stages, and the
// failure that matters -- the artifact is not listed in the manifest at all --
// arrives as an empty pipe rather than as an answer. Here it is an error that
// names what the manifest does list.
package main

import (
	"bufio"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/richardwooding/forge/sdk/tool"
)

type SumArgs struct {
	Data string `json:"data,omitempty" jsonschema:"the text to hash; give this or path, not both"`
	Path string `json:"path,omitempty" jsonschema:"a file to hash, instead of data; needs fs.read over its directory"`
	Algo string `json:"algo,omitempty" jsonschema:"sha256 (default), sha512, sha1 or md5"`
}

type SumOut struct {
	Hex   string `json:"hex"`
	Algo  string `json:"algo"`
	Bytes int64  `json:"bytes" jsonschema:"length of the input in bytes"`
	Path  string `json:"path,omitempty" jsonschema:"the file that was hashed, when one was"`
}

type VerifyArgs struct {
	Data      string `json:"data,omitempty" jsonschema:"the text to hash; give this or path, not both"`
	Path      string `json:"path,omitempty" jsonschema:"a file to hash, instead of data; needs fs.read over its directory"`
	Expected  string `json:"expected,omitempty" jsonschema:"the digest to compare against; case and surrounding whitespace are ignored"`
	Checksums string `json:"checksums,omitempty" jsonschema:"a sha256sum-style manifest to look the expected digest up in, instead of giving it directly; requires path"`
	Algo      string `json:"algo,omitempty" jsonschema:"defaults to whichever algorithm matches the length of expected"`
}

type VerifyOut struct {
	Match    bool   `json:"match"`
	Got      string `json:"got"`
	Expected string `json:"expected"`
	Algo     string `json:"algo"`
	Path     string `json:"path,omitempty" jsonschema:"the file that was hashed, when one was"`
	Source   string `json:"source,omitempty" jsonschema:"the manifest the expected digest was read from, when it was"`
	Note     string `json:"note,omitempty" jsonschema:"why the comparison could not be trusted, when it could not"`
}

var _ = tool.Register(
	tool.Spec{
		Name:    "hashsum",
		Version: "0.2.0",
		Summary: "Compute and verify message digests, over text or a file",
		Description: "Hashes text or a file and checks it against an expected digest, or against " +
			"a sha256sum-style checksums manifest. Reading a file needs fs.read; hashing text " +
			"needs nothing.",
		UseWhen: "Checking a download, a release artifact or a build output against a published " +
			"digest, or producing one. Never eyeball or hand-assemble a checksum comparison.",
		Labels: []string{"hash", "checksum", "file", "release"},
		Needs: []tool.Need{{
			Kind:  tool.FSRead,
			Scope: []string{"*"},
			Reason: "read the files you ask it to hash. It cannot know your directories, so " +
				"narrow this to the one you want: forge grant allow hashsum --scope fs.read=~/Downloads",
		}},
	},
	tool.Op("sum", sum,
		tool.Summary("Hash text or a file and return the hex digest"),
		tool.ReadOnly(), tool.Idempotent()),
	tool.Op("verify", verify,
		tool.Summary("Check text or a file against an expected digest, or against a checksums manifest"),
		tool.ReadOnly(), tool.Idempotent()),
)

func main() {}

// newHash returns the named algorithm. sha1 and md5 are here for reading old
// checksum files, not for anything that depends on collision resistance.
func newHash(algo string) (hash.Hash, string, error) {
	switch strings.ToLower(strings.TrimSpace(algo)) {
	case "", "sha256":
		return sha256.New(), "sha256", nil
	case "sha512":
		return sha512.New(), "sha512", nil
	case "sha1":
		return sha1.New(), "sha1", nil
	case "md5":
		return md5.New(), "md5", nil
	}
	return nil, "", fmt.Errorf("%q is not one of sha256, sha512, sha1 or md5", algo)
}

// algoForLength guesses the algorithm from a digest's length, so verifying a
// sha512 against a sha256 is caught as a mismatch of kind rather than
// reported as a plain false.
func algoForLength(n int) (string, bool) {
	switch n {
	case 64:
		return "sha256", true
	case 128:
		return "sha512", true
	case 40:
		return "sha1", true
	case 32:
		return "md5", true
	}
	return "", false
}

// digest hashes either a string or a file, whichever was given.
//
// The file is streamed rather than read whole: the artifacts worth hashing are
// the ones too big to want a second copy of in memory, and a tool that fell
// over on a large file would fail at precisely its reason for existing.
func digest(data, path, algo string) (hex string, name string, n int64, err error) {
	if (data == "") == (path == "") {
		if path == "" {
			return "", "", 0, errNeedInput
		}
		return "", "", 0, fmt.Errorf("give data or path, not both; hashing %q and some text at once has no meaning", path)
	}

	h, name, err := newHash(algo)
	if err != nil {
		return "", "", 0, err
	}

	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return "", "", 0, readErr(path, err)
		}
		defer f.Close() //nolint:errcheck // read-only
		n, err = io.Copy(h, f)
		if err != nil {
			return "", "", 0, fmt.Errorf("reading %s: %w", path, err)
		}
	} else {
		n = int64(len(data))
		h.Write([]byte(data))
	}
	return encode(h), name, n, nil
}

var errNeedInput = fmt.Errorf("give either data (text to hash) or path (a file to hash)")

// readErr turns an open failure into something that says what to do about it.
//
// A denied fs.read does not arrive as a permission error. forge does not mount
// what it has not granted, so from inside the guest an ungranted directory is
// simply absent, and the open fails with whatever wasip1 makes of a path under
// no mount -- ENOENT, or EBADF ("bad file number"), which is the one a reader
// would never connect to a missing grant. There is no separate signal to test
// for, so every open failure names the grant as a possibility rather than
// asserting the wrong cause.
func readErr(path string, err error) error {
	grant := fmt.Sprintf("forge grant allow hashsum --scope fs.read=%s", filepath.Dir(path))
	if os.IsNotExist(err) {
		return fmt.Errorf("%s: no such file. If it does exist, hashsum has not been granted "+
			"fs.read over its directory: %s", path, grant)
	}
	return fmt.Errorf("opening %s: %w. If the file is there, this is what an ungranted "+
		"directory looks like from inside the sandbox: %s", path, err, grant)
}

func encode(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

func sum(_ *tool.Context, a SumArgs) (SumOut, error) {
	hexsum, name, n, err := digest(a.Data, a.Path, a.Algo)
	if err != nil {
		return SumOut{}, err
	}
	return SumOut{Hex: hexsum, Algo: name, Bytes: n, Path: a.Path}, nil
}

func verify(_ *tool.Context, a VerifyArgs) (VerifyOut, error) {
	want, source, err := expectedDigest(a)
	if err != nil {
		return VerifyOut{}, err
	}

	algo := a.Algo
	guessed, guessOK := algoForLength(len(want))
	if algo == "" {
		if !guessOK {
			return VerifyOut{}, fmt.Errorf("a %d-character digest matches no algorithm hashsum knows; pass algo explicitly", len(want))
		}
		algo = guessed
	}

	got, name, _, err := digest(a.Data, a.Path, algo)
	if err != nil {
		return VerifyOut{}, err
	}

	out := VerifyOut{Match: got == want, Got: got, Expected: want, Algo: name, Path: a.Path, Source: source}
	if !out.Match && guessOK && guessed != name {
		out.Note = fmt.Sprintf("the expected digest is %d characters, which looks like %s rather than %s", len(want), guessed, name)
	}
	return out, nil
}

// expectedDigest resolves what to compare against: either the digest given
// directly, or the one this file is listed under in a checksums manifest.
func expectedDigest(a VerifyArgs) (want string, source string, err error) {
	switch {
	case a.Expected != "" && a.Checksums != "":
		return "", "", fmt.Errorf("give expected or checksums, not both; they are two ways of saying the same thing")

	case a.Checksums != "":
		if a.Path == "" {
			return "", "", fmt.Errorf("checksums needs path: the manifest is looked up by the file's name, and there is no name to look up")
		}
		want, err = lookup(a.Checksums, a.Path)
		if err != nil {
			return "", "", err
		}
		return want, a.Checksums, nil

	case a.Expected != "":
		want = strings.ToLower(strings.TrimSpace(a.Expected))
		// An empty expected digest is not a mismatch, it is a broken pipeline.
		// Reporting false here would say the data is wrong when what actually
		// failed was whatever was supposed to produce the digest.
		if want == "" {
			return "", "", fmt.Errorf("the expected digest is empty; whatever was meant to supply it produced nothing")
		}
		if _, err := hex.DecodeString(want); err != nil {
			return "", "", fmt.Errorf("the expected digest %q is not hexadecimal", a.Expected)
		}
		return want, "", nil
	}
	return "", "", fmt.Errorf("give expected (a digest) or checksums (a manifest to look one up in)")
}

// lookup finds path's digest in a sha256sum-style manifest.
//
// Not being listed is an error rather than a mismatch, and that is the whole
// reason this operation exists. The shell version of this check ends in
// `| sha256sum -c -`, and an artifact absent from the manifest reaches it as an
// empty pipe -- a shape that reports nothing useful about the thing actually
// asked. Naming the entries the manifest does have turns a silence into an
// answer, because in practice the cause is a renamed or misspelled artifact.
func lookup(manifest, path string) (string, error) {
	f, err := os.Open(manifest)
	if err != nil {
		return "", readErr(manifest, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	want := filepath.Base(path)
	found := map[string]string{}
	var names []string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		hexsum, name, ok := parseLine(sc.Text())
		if !ok {
			continue
		}
		names = append(names, name)
		if prior, seen := found[name]; seen && prior != hexsum {
			return "", fmt.Errorf("%s lists %s twice with different digests (%s and %s); it cannot be trusted",
				manifest, name, prior, hexsum)
		}
		found[name] = hexsum
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("reading %s: %w", manifest, err)
	}

	if hexsum, ok := found[want]; ok {
		return hexsum, nil
	}
	if len(names) == 0 {
		return "", fmt.Errorf("%s has no checksum lines in it at all", manifest)
	}
	sort.Strings(names)
	return "", fmt.Errorf("%s does not list %q. It lists: %s", manifest, want, strings.Join(names, ", "))
}

// parseLine reads one manifest line: a hex digest, whitespace, then a name.
//
// The name may carry a leading '*', which is how coreutils marks a file read in
// binary mode. Stripping it matters: goreleaser's checksums.txt does not use it
// and BSD's md5sum-alikes do, so a lookup that kept it would fail against half
// the manifests in the world for a reason nobody would guess from the message.
func parseLine(line string) (hexsum, name string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", false
	}
	hexsum = strings.ToLower(fields[0])
	if _, err := hex.DecodeString(hexsum); err != nil {
		return "", "", false
	}
	if _, known := algoForLength(len(hexsum)); !known {
		return "", "", false
	}
	name = strings.TrimPrefix(strings.Join(fields[1:], " "), "*")
	return hexsum, name, name != ""
}
