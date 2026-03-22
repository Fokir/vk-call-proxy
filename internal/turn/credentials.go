package turn

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// Credentials is an alias for provider.Credentials, kept for backward
// compatibility with code that references turn.Credentials.
type Credentials = provider.Credentials

// JoinResponse is an alias for provider.JoinInfo, kept for backward
// compatibility with code that references turn.JoinResponse.
type JoinResponse = provider.JoinInfo

// EnvProvider reads TURN credentials from environment variables
// (TURN_HOST, TURN_PORT, TURN_USERNAME, TURN_PASSWORD).
// Returns nil credentials if any variable is missing.
type EnvProvider struct{}

func (EnvProvider) FetchCredentials(_ context.Context) (*provider.Credentials, error) {
	host := os.Getenv("TURN_HOST")
	port := os.Getenv("TURN_PORT")
	user := os.Getenv("TURN_USERNAME")
	pass := os.Getenv("TURN_PASSWORD")

	if host == "" || port == "" || user == "" || pass == "" {
		return nil, nil
	}
	return &provider.Credentials{
		Username: user,
		Password: pass,
		Host:     host,
		Port:     port,
	}, nil
}

// CachedProvider returns pre-fetched credentials first, then falls back to
// the underlying provider when the cache is exhausted. This avoids expensive
// VK API calls on reconnect when the TURN credentials are still valid.
type CachedProvider struct {
	mu       sync.Mutex
	cache    []*provider.Credentials
	fallback provider.CredentialsProvider
}

// NewCachedProvider creates a provider that serves from cache first.
func NewCachedProvider(cached []*provider.Credentials, fallback provider.CredentialsProvider) *CachedProvider {
	cp := make([]*provider.Credentials, len(cached))
	copy(cp, cached)
	return &CachedProvider{cache: cp, fallback: fallback}
}

func (p *CachedProvider) FetchCredentials(ctx context.Context) (*provider.Credentials, error) {
	p.mu.Lock()
	if len(p.cache) > 0 {
		c := p.cache[0]
		p.cache = p.cache[1:]
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()
	return p.fallback.FetchCredentials(ctx)
}

// ParseTURNURL extracts host and port from a TURN URL like "turn:1.2.3.4:3478?transport=tcp".
func ParseTURNURL(turnURL string) (host, port string) {
	clean := strings.Split(turnURL, "?")[0]
	clean = strings.TrimPrefix(clean, "turn:")
	clean = strings.TrimPrefix(clean, "turns:")
	parts := strings.SplitN(clean, ":", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return clean, "3478"
}
