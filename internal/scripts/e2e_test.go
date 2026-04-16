package scripts

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// testServer serves a signed manifest and one file. It can be mutated at
// runtime to simulate rolling out an updated version.
type testServer struct {
	mu sync.Mutex

	priv      ed25519.PrivateKey
	scripts   map[string][]byte
	version   string
	etag      string
	mfBytes   []byte
	baseURL   string
	callCount int
}

func newTestServer(t *testing.T, files map[string][]byte) (*testServer, *httptest.Server) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	ts := &testServer{priv: priv, scripts: files}
	srv := httptest.NewServer(http.HandlerFunc(ts.serve))
	ts.baseURL = srv.URL
	ts.rebuild("v1")
	return ts, srv
}

func (ts *testServer) pub() string {
	return base64.StdEncoding.EncodeToString(ts.priv.Public().(ed25519.PublicKey))
}

func (ts *testServer) rebuild(version string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	scripts := map[string]map[string]any{}
	for name, data := range ts.scripts {
		sum := sha256.Sum256(data)
		scripts[name] = map[string]any{
			"url":    ts.baseURL + "/" + name,
			"sha256": hex.EncodeToString(sum[:]),
			"size":   int64(len(data)),
		}
	}
	mf := map[string]any{
		"version":      version,
		"published_at": time.Now().UTC().Format(time.RFC3339),
		"scripts":      scripts,
	}
	raw, _ := json.Marshal(mf)
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)

	// canonicalize same way as scripts-sign: re-marshal values compactly
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var payload []byte
	payload = append(payload, '{')
	for i, k := range keys {
		if i > 0 {
			payload = append(payload, ',')
		}
		kb, _ := json.Marshal(k)
		payload = append(payload, kb...)
		payload = append(payload, ':')
		var v any
		_ = json.Unmarshal(obj[k], &v)
		vb, _ := json.Marshal(v)
		payload = append(payload, vb...)
	}
	payload = append(payload, '}')

	sig := ed25519.Sign(ts.priv, payload)
	mf["signature"] = base64.StdEncoding.EncodeToString(sig)
	pretty, _ := json.MarshalIndent(mf, "", "  ")
	ts.mfBytes = pretty
	ts.version = version
	ts.etag = fmt.Sprintf(`"%s"`, version)
}

func (ts *testServer) serve(w http.ResponseWriter, r *http.Request) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.callCount++

	if r.URL.Path == "/manifest.json" {
		if r.Header.Get("If-None-Match") == ts.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", ts.etag)
		w.Write(ts.mfBytes)
		return
	}
	name := r.URL.Path[1:]
	if data, ok := ts.scripts[name]; ok {
		w.Write(data)
		return
	}
	http.NotFound(w, r)
}

func (ts *testServer) update(files map[string][]byte, version string) {
	ts.mu.Lock()
	ts.scripts = files
	ts.mu.Unlock()
	ts.rebuild(version)
}

func TestManager_DownloadsAndVerifies(t *testing.T) {
	ts, srv := newTestServer(t, map[string][]byte{
		"stealth.js":     []byte(`console.log("v1")`),
		"vk-config.json": []byte(`{"vk":{"api_version":"5.999"}}`),
	})
	defer srv.Close()

	dir := t.TempDir()
	m := NewManager(Config{
		URL:           ts.baseURL,
		PublicKey:     ts.pub(),
		LocalDir:      dir,
		CheckInterval: 50 * time.Millisecond,
		HTTPTimeout:   5 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop()

	// Wait for first download.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b := m.Current()
		if b != nil && b.Manifest != nil && b.Manifest.Version == "v1" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	b := m.Current()
	if b == nil || b.Manifest == nil || b.Manifest.Version != "v1" {
		t.Fatalf("expected v1 bundle, got %+v", b)
	}
	data, ok := b.File("stealth.js")
	if !ok || string(data) != `console.log("v1")` {
		t.Fatalf("stealth.js mismatch: %q", data)
	}
	cfg := m.VKConfig()
	if cfg == nil || cfg.VK.APIVersion != "5.999" {
		t.Fatalf("vk config mismatch: %+v", cfg)
	}

	// Update manifest and force-check.
	ts.update(map[string][]byte{
		"stealth.js":     []byte(`console.log("v2")`),
		"vk-config.json": []byte(`{"vk":{"api_version":"6.000"}}`),
	}, "v2")
	m.TriggerCheck()

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b := m.Current()
		if b != nil && b.Manifest != nil && b.Manifest.Version == "v2" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	b = m.Current()
	if b == nil || b.Manifest.Version != "v2" {
		t.Fatalf("expected v2 bundle, got %+v", b)
	}
	data, _ = b.File("stealth.js")
	if string(data) != `console.log("v2")` {
		t.Fatalf("stealth.js v2 mismatch: %q", data)
	}
}

func TestManager_StoreWatch_ReloadsOnSidecarWrite(t *testing.T) {
	ts, srv := newTestServer(t, map[string][]byte{
		"stealth.js": []byte(`v1`),
	})
	defer srv.Close()

	dir := t.TempDir()

	// Writer (simulates scripts-updater sidecar).
	writer := NewManager(Config{
		URL:           ts.baseURL,
		PublicKey:     ts.pub(),
		LocalDir:      dir,
		CheckInterval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := writer.Start(ctx); err != nil {
		t.Fatalf("writer start: %v", err)
	}
	defer writer.Stop()

	// Reader (simulates server/captcha container: no URL, same LocalDir).
	reader := NewManager(Config{
		URL:      "",
		LocalDir: dir,
	})
	if err := reader.Start(ctx); err != nil {
		t.Fatalf("reader start: %v", err)
	}
	defer reader.Stop()

	// Wait for writer's initial download to land in the store.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b := writer.Current(); b != nil && b.Manifest.Version == "v1" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Wait for reader to notice via store-watch.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b := reader.Current(); b != nil && b.Manifest != nil && b.Manifest.Version == "v1" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	rb := reader.Current()
	if rb == nil || rb.Manifest == nil || rb.Manifest.Version != "v1" {
		t.Fatalf("reader did not pick up v1: %+v", rb)
	}

	// Flip the writer to v2 — reader should see it without fetching itself.
	ts.update(map[string][]byte{"stealth.js": []byte(`v2`)}, "v2")
	writer.TriggerCheck()

	deadline = time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if b := reader.Current(); b != nil && b.Manifest != nil && b.Manifest.Version == "v2" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	rb = reader.Current()
	if rb == nil || rb.Manifest.Version != "v2" {
		t.Fatalf("reader did not reload to v2: %+v", rb)
	}
	if data, _ := rb.File("stealth.js"); string(data) != "v2" {
		t.Fatalf("reader stealth.js mismatch: %q", data)
	}
}

func TestManager_MinClientVersion_Skips(t *testing.T) {
	ts, srv := newTestServer(t, map[string][]byte{
		"stealth.js": []byte(`v1`),
	})
	defer srv.Close()

	// Inject min_client_version > 0.24.0 by re-signing with a custom mf.
	// Simpler: set the min via a second rebuild using mf modification.
	ts.mu.Lock()
	var mf map[string]any
	_ = json.Unmarshal(ts.mfBytes, &mf)
	mf["min_client_version"] = "99.0.0"
	delete(mf, "signature")
	raw, _ := json.Marshal(mf)
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var payload []byte
	payload = append(payload, '{')
	for i, k := range keys {
		if i > 0 {
			payload = append(payload, ',')
		}
		kb, _ := json.Marshal(k)
		payload = append(payload, kb...)
		payload = append(payload, ':')
		var v any
		_ = json.Unmarshal(obj[k], &v)
		vb, _ := json.Marshal(v)
		payload = append(payload, vb...)
	}
	payload = append(payload, '}')
	sig := ed25519.Sign(ts.priv, payload)
	mf["signature"] = base64.StdEncoding.EncodeToString(sig)
	ts.mfBytes, _ = json.MarshalIndent(mf, "", "  ")
	ts.etag = `"hi-min"`
	ts.mu.Unlock()

	dir := t.TempDir()
	m := NewManager(Config{
		URL:           ts.baseURL,
		PublicKey:     ts.pub(),
		LocalDir:      dir,
		ClientVersion: "0.24.1",
		CheckInterval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = m.Start(ctx)
	defer m.Stop()

	time.Sleep(500 * time.Millisecond)
	b := m.Current()
	if b != nil && b.Source == SourceRemote {
		t.Fatalf("client with ClientVersion=0.24.1 accepted manifest requiring 99.0.0: %+v", b)
	}
}

func TestManager_StatusAndRollback(t *testing.T) {
	ts, srv := newTestServer(t, map[string][]byte{
		"stealth.js": []byte(`v1`),
	})
	defer srv.Close()

	dir := t.TempDir()
	m := NewManager(Config{
		URL:           ts.baseURL,
		PublicKey:     ts.pub(),
		LocalDir:      dir,
		CheckInterval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = m.Start(ctx)
	defer m.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b := m.Current(); b != nil && b.Manifest != nil && b.Manifest.Version == "v1" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Move to v2 so we have something to roll back from.
	ts.update(map[string][]byte{"stealth.js": []byte(`v2`)}, "v2")
	m.TriggerCheck()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b := m.Current(); b != nil && b.Manifest != nil && b.Manifest.Version == "v2" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if v := m.Current().Manifest.Version; v != "v2" {
		t.Fatalf("pre-rollback version = %q", v)
	}

	// Trigger rollback by reporting 3 failures.
	for i := 0; i < failThreshold; i++ {
		m.ReportFailure("captcha")
	}
	time.Sleep(200 * time.Millisecond)

	b := m.Current()
	if b == nil || b.Manifest == nil || b.Manifest.Version != "v1" {
		t.Fatalf("expected rollback to v1, got %+v", b)
	}
	st := m.Status()
	if len(st.Quarantined) != 1 || st.Quarantined[0] != "v2" {
		t.Fatalf("expected quarantined [v2], got %v", st.Quarantined)
	}
	if st.Version != "v1" {
		t.Fatalf("status version = %q", st.Version)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.24.0", "0.24.1", -1},
		{"0.24.1", "0.24.0", 1},
		{"0.24.1", "0.24.1", 0},
		{"1.0", "0.24.999", 1},
		{"0.24.1", "0.24", 1},
		{"0.24", "0.24.0", 0},
		{"", "", 0},
	}
	for _, c := range cases {
		got := compareVersions(c.a, c.b)
		if got != c.want {
			t.Errorf("compareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestManager_RejectsBadSignature(t *testing.T) {
	ts, srv := newTestServer(t, map[string][]byte{
		"stealth.js": []byte(`x`),
	})
	defer srv.Close()

	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader)
	wrongPub := base64.StdEncoding.EncodeToString(wrongPriv.Public().(ed25519.PublicKey))

	dir := t.TempDir()
	m := NewManager(Config{
		URL:           ts.baseURL,
		PublicKey:     wrongPub, // mismatched public key
		LocalDir:      dir,
		CheckInterval: 50 * time.Millisecond,
		HTTPTimeout:   2 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = m.Start(ctx)
	defer m.Stop()

	time.Sleep(500 * time.Millisecond)

	// Should have bundled fallback (empty), NOT the downloaded content.
	b := m.Current()
	if b != nil && b.Manifest != nil && b.Source == SourceRemote {
		t.Fatalf("bad signature was accepted: %+v", b)
	}
	if _, err := filepath.Abs(dir); err != nil {
		t.Fatalf("tempdir: %v", err)
	}
}
