package hostsvc

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/richardwooding/forge/internal/capability"
)

// SecretAccess records one secret read, for the audit log.
type SecretAccess struct {
	Tool  string
	Name  string
	Found bool
}

// SecretsConfig configures the secret source.
type SecretsConfig struct {
	// Dir holds one file per secret, named for the secret.
	Dir string
	// Audit, when set, is called for every read -- including reads that find
	// nothing, because an unexpected lookup is as interesting as a successful
	// one. It must not block.
	Audit func(SecretAccess)
	// AllowLooseMode skips the permission check. Only for tests.
	AllowLooseMode bool
}

// Secrets reads secrets from files on disk.
//
// This is deliberately not the process environment. Environment variables are
// readable by everything in the process, cannot be audited per access, and
// would appear in the guest's own os.Environ() -- which would hand a tool
// every secret the moment it was granted any one of them.
type Secrets struct {
	cfg SecretsConfig

	// mu guards seen.
	mu sync.Mutex
	// seen holds the values handed out this process, so output can be redacted
	// before it reaches a log or an LLM's context.
	seen map[string]struct{}
}

// NewSecrets builds the source.
func NewSecrets(cfg SecretsConfig) *Secrets {
	return &Secrets{cfg: cfg, seen: make(map[string]struct{})}
}

// ErrSecretPermissions is returned when a secret file is readable by more than
// its owner.
var ErrSecretPermissions = errors.New("secret file has loose permissions")

// Secret implements hostabi.SecretSource.
func (s *Secrets) Secret(ctx context.Context, tool, name string) (string, bool, error) {
	clean, err := identifier("secret name", name)
	if err != nil {
		return "", false, err
	}

	path := filepath.Join(s.cfg.Dir, clean)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		s.audit(tool, name, false)
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading the secret %q: %w", name, err)
	}

	// A symlink here would let whoever can write the secrets directory point
	// forge at any file the user can read, and hand the contents to a tool.
	if info.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("the secret %q is a symlink; forge reads regular files only", name)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("the secret %q is not a regular file", name)
	}
	if !s.cfg.AllowLooseMode && info.Mode().Perm()&0o077 != 0 {
		return "", false, fmt.Errorf("%w: %s is %o; run chmod 600 on it",
			ErrSecretPermissions, path, info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("reading the secret %q: %w", name, err)
	}

	// A trailing newline is what an editor or `echo secret > file` leaves
	// behind, and is almost never part of the secret.
	value := strings.TrimRight(string(data), "\r\n")

	s.mu.Lock()
	if value != "" {
		s.seen[value] = struct{}{}
	}
	s.mu.Unlock()

	s.audit(tool, name, true)
	return value, true, nil
}

func (s *Secrets) audit(tool, name string, found bool) {
	if s.cfg.Audit != nil {
		s.cfg.Audit(SecretAccess{Tool: tool, Name: name, Found: found})
	}
}

// Redact replaces every secret value handed out so far with a placeholder.
//
// A tool holding both secret and net.http can exfiltrate deliberately and no
// amount of redaction stops it -- that combination is flagged at approval time
// instead. What this does prevent is the ordinary accident: a secret echoed
// into an error message, a build log, or an LLM's context window.
func (s *Secrets) Redact(text string) string {
	s.mu.Lock()
	values := make([]string, 0, len(s.seen))
	for v := range s.seen {
		values = append(values, v)
	}
	s.mu.Unlock()

	// Longest first, so a secret that contains another is replaced whole
	// rather than being broken up by the shorter one.
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && len(values[j]) > len(values[j-1]); j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
	for _, v := range values {
		// Very short secrets are skipped: replacing every "a" in the output
		// would be worse than the leak it guards against.
		if len(v) < 6 {
			continue
		}
		text = strings.ReplaceAll(text, v, "[redacted]")
	}
	return text
}

// Names lists the secrets on disk, for `forge secret list`. Values are never
// read here.
func (s *Secrets) Names() ([]string, error) {
	entries, err := os.ReadDir(s.cfg.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing secrets: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, err := identifier("secret name", e.Name()); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// Put writes a secret, creating the directory 0700 and the file 0600.
func (s *Secrets) Put(name, value string) error {
	clean, err := identifier("secret name", name)
	if err != nil {
		return err
	}
	if err := writePrivate(s.cfg.Dir, clean, value); err != nil {
		return fmt.Errorf("writing the secret: %w", err)
	}
	return nil
}

// writePrivate writes a file atomically, readable only by its owner.
func writePrivate(dir, name, value string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	// Chmod before the write, so the value is never on disk world-readable,
	// even briefly.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(value); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, name))
}

// Remove deletes a secret and its binding. Removing one that is not there is
// not an error.
func (s *Secrets) Remove(name string) error {
	clean, err := identifier("secret name", name)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.cfg.Dir, clean)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the secret: %w", err)
	}
	return s.Unbind(clean)
}

// bindingDir holds one file per bound secret. It is a directory, so Names
// never lists it, and identifier refuses a slash, so no secret name reaches it.
func (s *Secrets) bindingDir() string { return filepath.Join(s.cfg.Dir, ".hosts") }

// Bind restricts a secret to the given host patterns, replacing any earlier
// binding. The secret need not exist yet: binding before storing is how a new
// token avoids ever being on disk unbound.
func (s *Secrets) Bind(name string, hosts []string) error {
	clean, err := identifier("secret name", name)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return errors.New("a binding needs at least one host")
	}
	patterns := make([]string, 0, len(hosts))
	for _, h := range hosts {
		p, err := hostPattern(h)
		if err != nil {
			return err
		}
		if !slices.Contains(patterns, p) {
			patterns = append(patterns, p)
		}
	}
	slices.Sort(patterns)
	return writePrivate(s.bindingDir(), clean, strings.Join(patterns, "\n")+"\n")
}

// Unbind lifts a secret's binding. Unbinding one that is not bound is not an
// error.
func (s *Secrets) Unbind(name string) error {
	clean, err := identifier("secret name", name)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.bindingDir(), clean)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the binding: %w", err)
	}
	return nil
}

// Hosts returns the patterns a secret is bound to, or nil when it is unbound.
func (s *Secrets) Hosts(name string) ([]string, error) {
	clean, err := identifier("secret name", name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(s.bindingDir(), clean)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the binding for %q: %w", name, err)
	}
	// Unreadable or tampered with is not the same as unbound: failing open here
	// would hand a bound token to whichever host asked.
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("the binding for %q is not a regular file", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the binding for %q: %w", name, err)
	}
	var hosts []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			hosts = append(hosts, line)
		}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("the binding for %q is empty; rebind it or run forge secret unbind %s", name, name)
	}
	return hosts, nil
}

// Bound implements hostabi.SecretSource.
func (s *Secrets) Bound(name string) (bool, error) {
	hosts, err := s.Hosts(name)
	return len(hosts) > 0, err
}

// Credential implements hostabi.CredentialSource.
//
// The binding is checked before the secret is read, so a refused request
// leaves no read in the audit log for a value that was never used.
func (s *Secrets) Credential(ctx context.Context, tool, name, host string) (string, error) {
	hosts, err := s.Hosts(name)
	if err != nil {
		return "", err
	}
	if len(hosts) > 0 {
		bound := capability.NewSet(capability.Grant{Kind: capability.NetHTTP, Scope: hosts})
		if !bound.Allow(capability.NetHTTP, host).OK {
			return "", capability.Refusef(capability.DenyOutOfScope,
				"secret %q is bound to %s and may not be sent to %s", name, strings.Join(hosts, ", "), host)
		}
	}
	v, found, err := s.Secret(ctx, tool, name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("the secret %q is not set; add it with: forge secret set %s", name, name)
	}
	return v, nil
}

// CredentialValue renders a secret as the value of the header it is attached
// to.
func CredentialValue(header, scheme, secret string) (string, error) {
	if scheme == "" && strings.EqualFold(header, "Authorization") {
		scheme = "Bearer"
	}
	switch strings.ToLower(scheme) {
	case "":
		return secret, nil
	case "basic":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(secret)), nil
	case "bearer":
		return "Bearer " + secret, nil
	}
	return "", fmt.Errorf("unknown credential scheme %q; use Bearer, Basic or none", scheme)
}

// hostPattern validates a binding pattern: a host name, optionally with one
// leading "*." label, as net.http grants use. A port, scheme or path is
// refused rather than stripped, since a binding someone mistyped should fail
// where they typed it.
func hostPattern(raw string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	name := strings.TrimPrefix(p, "*.")
	if name == "" || strings.ContainsAny(name, "*:/@?# ") {
		return "", fmt.Errorf("%q is not a host; give a name such as api.example.com or *.example.com", raw)
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("%q is not a host name", raw)
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", fmt.Errorf("%q is not a host name", raw)
			}
		}
	}
	return p, nil
}
