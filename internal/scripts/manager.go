package scripts

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type Manager struct {
	cfg     Config
	fetcher *fetcher
	store   *Store
	bundled fs.FS

	current atomic.Pointer[Bundle]

	forceCh chan struct{}
	stopCh  chan struct{}
	wg      sync.WaitGroup

	failMu       sync.Mutex
	recentFails  []time.Time
	quarantined  map[string]bool
	lastForceAt  time.Time
}

const (
	failWindow         = 5 * time.Minute
	failThreshold      = 3
	forceCheckDebounce = 30 * time.Second
	storePollInterval  = 2 * time.Second
)

func NewManager(cfg Config) *Manager {
	cfg = ResolveConfig(cfg)
	bundled := cfg.Bundled
	if bundled == nil {
		bundled = DefaultBundledFS()
	}
	m := &Manager{
		cfg:         cfg,
		fetcher:     newFetcher(cfg),
		bundled:     bundled,
		forceCh:     make(chan struct{}, 1),
		stopCh:      make(chan struct{}),
		quarantined: make(map[string]bool),
	}
	if cfg.LocalDir != "" {
		m.store = NewStore(cfg.LocalDir, cfg.Logger)
	}
	return m
}

func (m *Manager) Start(ctx context.Context) error {
	if err := m.loadInitial(); err != nil {
		m.cfg.Logger.Warnf("scripts: initial load: %v", err)
	}
	if m.cfg.URL != "" {
		m.wg.Add(1)
		go m.runLoop(ctx)
	} else {
		m.cfg.Logger.Infof("scripts: remote URL not configured; running in read-only mode")
	}
	if m.store != nil {
		m.wg.Add(1)
		go m.watchStore(ctx)
	}
	return nil
}

// watchStore polls the on-disk manifest.json mtime and reloads the bundle
// from disk when another process (e.g. a sidecar updater) writes a new one.
// This keeps multiple containers sharing the same volume in sync.
func (m *Manager) watchStore(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(storePollInterval)
	defer t.Stop()

	var lastMtime time.Time
	manifestPath := filepath.Join(m.store.currentDir(), manifestFile)

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-t.C:
		}
		st, err := os.Stat(manifestPath)
		if err != nil {
			continue
		}
		if st.ModTime().Equal(lastMtime) {
			continue
		}
		lastMtime = st.ModTime()
		b, err := m.store.LoadCurrent()
		if err != nil {
			m.cfg.Logger.Warnf("scripts: reload from store: %v", err)
			continue
		}
		cur := m.Current()
		if cur != nil && cur.Manifest != nil && b.Manifest != nil &&
			cur.Manifest.Version == b.Manifest.Version {
			continue
		}
		m.current.Store(b)
		if b.Manifest != nil {
			m.cfg.Logger.Infof("scripts: reloaded from store to %s", b.Manifest.Version)
		}
	}
}

func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
	m.wg.Wait()
}

func (m *Manager) Current() *Bundle {
	return m.current.Load()
}

func (m *Manager) File(name string) ([]byte, bool) {
	b := m.Current()
	if b == nil {
		return nil, false
	}
	return b.File(name)
}

func (m *Manager) ReportFailure(scriptID string) {
	m.failMu.Lock()
	now := time.Now()
	cutoff := now.Add(-failWindow)
	kept := m.recentFails[:0]
	for _, t := range m.recentFails {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	m.recentFails = kept
	overThreshold := len(kept) >= failThreshold
	var curVersion string
	if b := m.Current(); b != nil && b.Manifest != nil {
		curVersion = b.Manifest.Version
	}
	shouldRollback := overThreshold && m.store != nil && curVersion != ""
	if shouldRollback {
		m.quarantined[curVersion] = true
		m.recentFails = nil
	}
	m.failMu.Unlock()

	m.cfg.Logger.Warnf("scripts: failure reported for %q (recent=%d)", scriptID, len(kept))
	if shouldRollback {
		m.cfg.Logger.Errorf("scripts: quarantining %s and rolling back", curVersion)
		if prev, err := m.store.Rollback(); err != nil {
			m.cfg.Logger.Errorf("scripts: rollback failed: %v", err)
		} else if prev != nil {
			m.current.Store(prev)
			if prev.Manifest != nil {
				m.cfg.Logger.Infof("scripts: rolled back to %s", prev.Manifest.Version)
			}
		}
		return
	}
	if overThreshold {
		m.TriggerCheck()
	}
}

// Status returns a snapshot suitable for a /healthz endpoint.
type Status struct {
	Version       string    `json:"version"`
	Source        string    `json:"source"`
	LastCheck     time.Time `json:"last_check"`
	HasRemoteURL  bool      `json:"has_remote_url"`
	RecentFailures int      `json:"recent_failures"`
	Quarantined    []string `json:"quarantined,omitempty"`
}

func (m *Manager) Status() Status {
	s := Status{HasRemoteURL: m.cfg.URL != ""}
	if b := m.Current(); b != nil && b.Manifest != nil {
		s.Version = b.Manifest.Version
		s.Source = b.Source.String()
	}
	m.failMu.Lock()
	s.LastCheck = m.lastForceAt
	s.RecentFailures = len(m.recentFails)
	for v := range m.quarantined {
		s.Quarantined = append(s.Quarantined, v)
	}
	m.failMu.Unlock()
	return s
}

func (m *Manager) TriggerCheck() {
	m.failMu.Lock()
	if time.Since(m.lastForceAt) < forceCheckDebounce {
		m.failMu.Unlock()
		return
	}
	m.lastForceAt = time.Now()
	m.failMu.Unlock()

	select {
	case m.forceCh <- struct{}{}:
	default:
	}
}

func (m *Manager) loadInitial() error {
	if m.store != nil {
		if b, err := m.store.LoadCurrent(); err == nil {
			m.current.Store(b)
			m.cfg.Logger.Infof("scripts: loaded local bundle %s", b.Manifest.Version)
			return nil
		}
	}
	b, err := LoadBundled(m.bundled)
	if err != nil {
		return err
	}
	m.current.Store(b)
	m.cfg.Logger.Infof("scripts: loaded bundled %s", b.Manifest.Version)

	if m.store != nil {
		if raw, err := fs.ReadFile(m.bundled, manifestFile); err == nil {
			_ = m.store.Swap(b, raw)
		}
	}
	return nil
}

func (m *Manager) runLoop(ctx context.Context) {
	defer m.wg.Done()
	initial := m.cfg.CheckInterval
	if initial > time.Second {
		initial = time.Second
	}
	if initial <= 0 {
		initial = 100 * time.Millisecond
	}
	t := time.NewTimer(initial)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-t.C:
			m.tryUpdate(ctx)
			t.Reset(m.cfg.CheckInterval)
		case <-m.forceCh:
			m.tryUpdate(ctx)
			t.Reset(m.cfg.CheckInterval)
		}
	}
}

func (m *Manager) tryUpdate(ctx context.Context) {
	raw, etag, err := m.fetcher.fetchManifest(ctx)
	if errors.Is(err, ErrNotModified) {
		return
	}
	if err != nil {
		m.cfg.Logger.Warnf("scripts: fetch manifest: %v", err)
		return
	}
	mf, err := verifyManifest(raw, m.cfg.PublicKey)
	if err != nil {
		m.cfg.Logger.Errorf("scripts: manifest verify: %v", err)
		return
	}
	m.failMu.Lock()
	quarantined := m.quarantined[mf.Version]
	m.failMu.Unlock()
	if quarantined {
		m.cfg.Logger.Warnf("scripts: manifest %s is quarantined", mf.Version)
		return
	}
	if m.cfg.ClientVersion != "" && mf.MinClientVersion != "" {
		if compareVersions(m.cfg.ClientVersion, mf.MinClientVersion) < 0 {
			m.cfg.Logger.Warnf("scripts: manifest %s requires client >= %s (have %s); skipping",
				mf.Version, mf.MinClientVersion, m.cfg.ClientVersion)
			return
		}
	}
	cur := m.Current()
	if cur != nil && cur.Manifest != nil && cur.Manifest.Version == mf.Version {
		m.fetcher.commitETag(etag)
		return
	}

	files := make(map[string][]byte, len(mf.Scripts))
	for name, entry := range mf.Scripts {
		data, err := m.fetcher.fetchScript(ctx, entry)
		if err != nil {
			m.cfg.Logger.Errorf("scripts: fetch %s: %v", name, err)
			return
		}
		files[name] = data
	}
	newBundle := &Bundle{Manifest: mf, Files: files, Source: SourceRemote}
	if m.store != nil {
		if err := m.store.Swap(newBundle, raw); err != nil {
			m.cfg.Logger.Errorf("scripts: store swap: %v", err)
			return
		}
	}
	m.current.Store(newBundle)
	m.fetcher.commitETag(etag)
	m.cfg.Logger.Infof("scripts: updated to %s", mf.Version)
}
