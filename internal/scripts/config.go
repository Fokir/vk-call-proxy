package scripts

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"
)

// DefaultURL and DefaultPublicKey are populated from environment variables
// CALLVPN_SCRIPTS_URL and CALLVPN_SCRIPTS_PUBKEY respectively, or can be
// overridden at build time via ldflags.
var (
	DefaultURL       = ""
	DefaultPublicKey = ""
)

type Config struct {
	URL           string
	PublicKey     string
	LocalDir      string
	CheckInterval time.Duration
	HTTPTimeout   time.Duration
	Bundled       fs.FS
	UserAgent     string
	Logger        Logger
	// ClientVersion is the current binary's version. If non-empty, manifests
	// with min_client_version > ClientVersion are skipped. Accepts semver-ish
	// strings like "0.24.1".
	ClientVersion string
}

type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

func ResolveConfig(override Config) Config {
	c := override

	if c.URL == "" {
		c.URL = firstNonEmpty(os.Getenv("CALLVPN_SCRIPTS_URL"), DefaultURL)
	}
	if c.PublicKey == "" {
		c.PublicKey = firstNonEmpty(os.Getenv("CALLVPN_SCRIPTS_PUBKEY"), DefaultPublicKey)
	}
	if c.CheckInterval == 0 {
		c.CheckInterval = 1 * time.Hour
	}
	if c.HTTPTimeout == 0 {
		c.HTTPTimeout = 30 * time.Second
	}
	if c.UserAgent == "" {
		c.UserAgent = "callvpn-scripts/1"
	}
	if c.Logger == nil {
		c.Logger = noopLogger{}
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

type noopLogger struct{}

func (noopLogger) Infof(string, ...any)  {}
func (noopLogger) Warnf(string, ...any)  {}
func (noopLogger) Errorf(string, ...any) {}

// NewSlogLogger wraps a *slog.Logger as a scripts.Logger. If log is nil a
// no-op logger is returned.
func NewSlogLogger(log *slog.Logger) Logger {
	if log == nil {
		return noopLogger{}
	}
	return slogLogger{log: log}
}

type slogLogger struct{ log *slog.Logger }

func (s slogLogger) Infof(format string, args ...any) {
	s.log.Info(fmt.Sprintf(format, args...))
}

func (s slogLogger) Warnf(format string, args ...any) {
	s.log.Warn(fmt.Sprintf(format, args...))
}

func (s slogLogger) Errorf(format string, args ...any) {
	s.log.Error(fmt.Sprintf(format, args...))
}
