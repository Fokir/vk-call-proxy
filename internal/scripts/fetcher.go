package scripts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const maxScriptSize = 8 * 1024 * 1024

var ErrNotModified = errors.New("scripts: not modified")

type fetcher struct {
	client    *http.Client
	baseURL   string
	userAgent string
	log       Logger

	manifestETag string
}

func newFetcher(cfg Config) *fetcher {
	return &fetcher{
		client:    &http.Client{Timeout: cfg.HTTPTimeout},
		baseURL:   cfg.URL,
		userAgent: cfg.UserAgent,
		log:       cfg.Logger,
	}
}

func (f *fetcher) manifestURL() string {
	return f.baseURL + "/manifest.json"
}

func (f *fetcher) fetchManifest(ctx context.Context) ([]byte, string, error) {
	if f.baseURL == "" {
		return nil, "", errors.New("scripts: base URL not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.manifestURL(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", f.userAgent)
	if f.manifestETag != "" {
		req.Header.Set("If-None-Match", f.manifestETag)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, f.manifestETag, ErrNotModified
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("scripts: manifest HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxScriptSize))
	if err != nil {
		return nil, "", err
	}
	etag := resp.Header.Get("ETag")
	return data, etag, nil
}

func (f *fetcher) fetchScript(ctx context.Context, entry ScriptEntry) ([]byte, error) {
	url := entry.URL
	if url == "" {
		return nil, errors.New("scripts: empty entry URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.userAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scripts: fetch %s HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxScriptSize))
	if err != nil {
		return nil, err
	}
	if err := verifyScript(data, entry.SHA256); err != nil {
		return nil, err
	}
	return data, nil
}

func (f *fetcher) commitETag(etag string) {
	if etag != "" {
		f.manifestETag = etag
	}
}

func backoffDelay(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * time.Second
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}
