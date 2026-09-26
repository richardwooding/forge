package hostsvc

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// KVConfig configures the key/value store.
type KVConfig struct {
	// Dir is the root beneath which namespaces live.
	Dir string
	// MaxValueBytes caps one value.
	MaxValueBytes int64
	// MaxKeys caps the keys in one namespace, so a looping tool cannot fill
	// the disk one small file at a time.
	MaxKeys int
}

func (c KVConfig) withDefaults() KVConfig {
	if c.MaxValueBytes == 0 {
		c.MaxValueBytes = 1 << 20
	}
	if c.MaxKeys == 0 {
		c.MaxKeys = 10000
	}
	return c
}

// KV is a per-tool, per-namespace key/value store on disk.
//
// The layout is <dir>/<tool>/<namespace>/<encoded key>. Tool and namespace are
// validated as plain identifiers and the key is base64url-encoded, so no part
// of a guest-supplied string is ever interpreted as a path -- there is no
// traversal to defend against because no separator can survive the encoding.
type KV struct {
	cfg KVConfig

	// mu makes a read-modify-write of one key atomic with respect to other
	// invocations in this process. Two instances of a tool can run at once on
	// different surfaces, and both reach the same files.
	mu sync.Mutex
}

// NewKV builds the store.
func NewKV(cfg KVConfig) *KV { return &KV{cfg: cfg.withDefaults()} }

// ErrValueTooLarge is returned when a value exceeds the configured cap.
var ErrValueTooLarge = errors.New("value is too large")

// ErrTooManyKeys is returned when a namespace is full.
var ErrTooManyKeys = errors.New("namespace holds too many keys")

func (k *KV) dir(tool, namespace string) (string, error) {
	t, err := identifier("tool", tool)
	if err != nil {
		return "", err
	}
	n, err := identifier("namespace", namespace)
	if err != nil {
		return "", err
	}
	return filepath.Join(k.cfg.Dir, t, n), nil
}

// encodeKey makes a guest string safe to use as a filename, and reversible so
// List can report the keys a tool actually wrote.
func encodeKey(key string) (string, error) {
	if key == "" {
		return "", errors.New("the key is empty")
	}
	enc := base64.RawURLEncoding.EncodeToString([]byte(key))
	// Most filesystems stop at 255 bytes for one name.
	if len(enc) > 200 {
		return "", fmt.Errorf("the key is too long (%d bytes; the limit is 150)", len(key))
	}
	return enc, nil
}

func decodeKey(name string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(name)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// Get implements hostabi.KVStore.
func (k *KV) Get(ctx context.Context, tool, namespace, key string) ([]byte, bool, error) {
	dir, err := k.dir(tool, namespace)
	if err != nil {
		return nil, false, err
	}
	enc, err := encodeKey(key)
	if err != nil {
		return nil, false, err
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	data, err := os.ReadFile(filepath.Join(dir, enc))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading %q: %w", key, err)
	}
	return data, true, nil
}

// Set implements hostabi.KVStore.
func (k *KV) Set(ctx context.Context, tool, namespace, key string, value []byte) error {
	if int64(len(value)) > k.cfg.MaxValueBytes {
		return fmt.Errorf("%w: %d bytes, the limit is %d", ErrValueTooLarge, len(value), k.cfg.MaxValueBytes)
	}
	dir, err := k.dir(tool, namespace)
	if err != nil {
		return err
	}
	enc, err := encodeKey(key)
	if err != nil {
		return err
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the namespace: %w", err)
	}
	target := filepath.Join(dir, enc)
	if _, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) {
		// Only count when adding a key; overwriting an existing one is always
		// allowed, so a full namespace stays usable for what is already in it.
		n, err := countEntries(dir)
		if err != nil {
			return err
		}
		if n >= k.cfg.MaxKeys {
			return fmt.Errorf("%w: %d keys, the limit is %d", ErrTooManyKeys, n, k.cfg.MaxKeys)
		}
	}

	// Write to a sibling and rename, so a crash or a concurrent reader never
	// sees a half-written value.
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("writing %q: %w", key, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(value); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %q: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %q: %w", key, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("writing %q: %w", key, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("writing %q: %w", key, err)
	}
	return nil
}

// Delete implements hostabi.KVStore. Removing a key that is not there is not
// an error -- the caller's intent is satisfied either way.
func (k *KV) Delete(ctx context.Context, tool, namespace, key string) error {
	dir, err := k.dir(tool, namespace)
	if err != nil {
		return err
	}
	enc, err := encodeKey(key)
	if err != nil {
		return err
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	if err := os.Remove(filepath.Join(dir, enc)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("deleting %q: %w", key, err)
	}
	return nil
}

// List implements hostabi.KVStore, returning the keys with the given prefix in
// sorted order.
func (k *KV) List(ctx context.Context, tool, namespace, prefix string) ([]string, error) {
	dir, err := k.dir(tool, namespace)
	if err != nil {
		return nil, err
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing the namespace: %w", err)
	}

	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		key, ok := decodeKey(e.Name())
		if !ok {
			continue // not ours; leave it alone rather than reporting nonsense
		}
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func countEntries(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("counting the namespace: %w", err)
	}
	return len(entries), nil
}

// identifier accepts the conservative shape forge uses for tool and namespace
// names. It is deliberately stricter than "contains no separator": a name that
// is a plain identifier cannot be "..", cannot be absolute, and cannot be a
// Windows device or an ADS.
func identifier(what, s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("the %s is empty", what)
	}
	if len(s) > 128 {
		return "", fmt.Errorf("the %s is too long", what)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return "", fmt.Errorf("the %s %q may only hold letters, digits, '-', '_' and '.'", what, s)
		}
	}
	if s == "." || s == ".." {
		return "", fmt.Errorf("%q is not a usable %s", s, what)
	}
	return s, nil
}
