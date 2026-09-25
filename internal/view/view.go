// Package view stores named label selectors.
//
// A view is a saved answer to "which of my tools am I working with right now".
// It matters most on the MCP surface, where every tool in the list costs an
// agent context on every single request, but it is useful anywhere: a person
// with forty tools installed does not want forty subcommands in their help.
package view

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	"github.com/richardwooding/forge/internal/labels"
)

// ErrNotFound is returned for a view that does not exist.
var ErrNotFound = errors.New("view not found")

// nameRE is the charset for a view name. It matches the label charset, so a
// view name can also appear in a URL path -- the MCP surface mounts one server
// per view at /mcp/{view}, and a name that needed escaping there would be a
// trap waiting for whoever wrote the first view called "my tools".
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// View is a named selector.
type View struct {
	Name     string `json:"-"`
	Selector string `json:"selector"`
}

type config struct {
	// Active is the view used when none is named. Empty means everything.
	Active string          `json:"active,omitempty"`
	Views  map[string]View `json:"views,omitempty"`
}

// Store persists views in a single JSON file.
type Store struct {
	path string

	mu  sync.RWMutex
	cfg config
}

// Open reads the views file, creating nothing until something is written.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "views.json")}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.cfg = config{Views: map[string]View{}}
			return nil
		}
		return err
	}
	if err := json.Unmarshal(b, &s.cfg); err != nil {
		return fmt.Errorf("reading %s: %w", s.path, err)
	}
	if s.cfg.Views == nil {
		s.cfg.Views = map[string]View{}
	}
	return nil
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path, append(b, '\n'))
}

// Set creates or replaces a view. The selector is parsed before it is stored,
// so a malformed one is rejected where it is typed rather than the next time
// forge starts.
func (s *Store) Set(name, selector string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("view name %q must match %s", name, nameRE)
	}
	sel, err := labels.Parse(selector)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Stored in canonical form, which round-trips: a saved view means the same
	// thing every time it is loaded.
	s.cfg.Views[name] = View{Name: name, Selector: sel.String()}
	return s.save()
}

// Delete removes a view, and clears the active view if it was this one.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cfg.Views[name]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	delete(s.cfg.Views, name)
	if s.cfg.Active == name {
		// Leaving a dangling active view would make every later command fail
		// with "view not found" until the user worked out why.
		s.cfg.Active = ""
	}
	return s.save()
}

// Get returns one view.
func (s *Store) Get(name string) (View, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.cfg.Views[name]
	if !ok {
		return View{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	v.Name = name
	return v, nil
}

// List returns every view, ordered by name.
func (s *Store) List() []View {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]View, 0, len(s.cfg.Views))
	for name, v := range s.cfg.Views {
		v.Name = name
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SetActive chooses the default view. An empty name means everything.
func (s *Store) SetActive(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name != "" {
		if _, ok := s.cfg.Views[name]; !ok {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
	}
	s.cfg.Active = name
	return s.save()
}

// ActiveName is the current default view, or "" for everything.
func (s *Store) ActiveName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Active
}

// Resolve turns a request into a selector.
//
// Precedence is explicit selector, then named view, then the active view, then
// everything. A named view that does not exist is an error rather than a
// silent fall back to everything: a typo in --view must not quietly widen what
// is exposed, least of all on a surface where the view is the only boundary.
func (s *Store) Resolve(name, selector string) (labels.Selector, string, error) {
	if selector != "" {
		sel, err := labels.Parse(selector)
		return sel, "", err
	}
	if name == "" {
		name = s.ActiveName()
	}
	if name == "" {
		return labels.All, "", nil
	}
	v, err := s.Get(name)
	if err != nil {
		return nil, "", err
	}
	sel, err := labels.Parse(v.Selector)
	if err != nil {
		return nil, "", fmt.Errorf("view %q holds an unparseable selector %q: %w", name, v.Selector, err)
	}
	return sel, name, nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
