package scripts

import (
	"flag"
	"os"
	"path/filepath"
	"time"
)

// Flags holds pointers to flag-bound strings. Populate via RegisterFlags and
// pass to BuildConfig once flag.Parse has been called.
type Flags struct {
	URL       *string
	PublicKey *string
	LocalDir  *string
	Interval  *time.Duration
}

// RegisterFlags adds scripts-related flags to the given flagset with sensible
// defaults. Empty default URL means the manager falls back to DefaultURL
// (compile-time via ldflags) and then to bundled only.
func RegisterFlags(fs *flag.FlagSet) Flags {
	return Flags{
		URL:       fs.String("scripts-url", "", "base URL to fetch hot-update scripts from (overrides CALLVPN_SCRIPTS_URL and ldflags default)"),
		PublicKey: fs.String("scripts-pubkey", "", "base64 Ed25519 public key (overrides CALLVPN_SCRIPTS_PUBKEY and ldflags default)"),
		LocalDir:  fs.String("scripts-dir", "", "local directory to store downloaded scripts (defaults to $CWD/var/scripts or $TMP/callvpn-scripts)"),
		Interval:  fs.Duration("scripts-interval", 0, "how often to check for script updates (default 1h)"),
	}
}

// BuildConfig resolves a Config from parsed flags, env, and defaults.
// Pass logger separately.
func (f Flags) BuildConfig(logger Logger) Config {
	cfg := Config{Logger: logger}
	if f.URL != nil {
		cfg.URL = *f.URL
	}
	if f.PublicKey != nil {
		cfg.PublicKey = *f.PublicKey
	}
	if f.LocalDir != nil {
		cfg.LocalDir = *f.LocalDir
	}
	if f.Interval != nil {
		cfg.CheckInterval = *f.Interval
	}
	if cfg.LocalDir == "" {
		cfg.LocalDir = DefaultLocalDir()
	}
	return cfg
}

// DefaultLocalDir returns a reasonable default for the local scripts cache:
// `$CALLVPN_SCRIPTS_DIR` if set, otherwise `./var/scripts` when writable,
// otherwise the OS temp dir.
func DefaultLocalDir() string {
	if env := os.Getenv("CALLVPN_SCRIPTS_DIR"); env != "" {
		return env
	}
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, "var", "scripts")
		if err := os.MkdirAll(candidate, 0o755); err == nil {
			return candidate
		}
	}
	return filepath.Join(os.TempDir(), "callvpn-scripts")
}
