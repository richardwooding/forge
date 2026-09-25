package toolkit

import (
	"os"
	"path/filepath"
)

// Paths are the directories forge uses. They follow the XDG layout, because
// that is what the rest of this machine does and because keeping data, cache
// and config apart means a user can delete the cache without losing their
// tools.
type Paths struct {
	// Data holds installed tools: the thing that is painful to lose.
	Data string
	// Cache holds compiled modules and the Go build cache: always rebuildable.
	Cache string
	// Config holds views and the capability policy.
	Config string
}

// DefaultPaths resolves forge's directories, honouring the XDG variables and
// a FORGE_HOME override that puts everything in one place.
func DefaultPaths() Paths {
	if home := os.Getenv("FORGE_HOME"); home != "" {
		return Paths{
			Data:   filepath.Join(home, "data"),
			Cache:  filepath.Join(home, "cache"),
			Config: filepath.Join(home, "config"),
		}
	}
	return Paths{
		Data:   filepath.Join(xdg("XDG_DATA_HOME", ".local", "share"), "forge"),
		Cache:  filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "forge"),
		Config: filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "forge"),
	}
}

func xdg(env string, fallback ...string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(append([]string{home}, fallback...)...)
}
