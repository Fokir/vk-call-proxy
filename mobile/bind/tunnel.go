// Package bind provides gomobile bindings for the VPN tunnel core.
// Build with: gomobile bind -target android ./mobile/bind/
//             gomobile bind -target ios ./mobile/bind/
package bind

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/call-vpn/call-vpn/internal/captcha"
	"github.com/call-vpn/call-vpn/internal/client"
	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/telemost"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
	"github.com/call-vpn/call-vpn/internal/scripts"
	"github.com/call-vpn/call-vpn/internal/scriptshook"
	"github.com/call-vpn/call-vpn/internal/speedtest"
	"github.com/call-vpn/call-vpn/internal/tunnel"
	"github.com/call-vpn/call-vpn/internal/turn"
	"github.com/google/uuid"
)

// LogBuffer is a thread-safe ring buffer that implements io.Writer.
// It stores the last N log lines for retrieval by native mobile code.
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	cap   int
}

// NewLogBuffer creates a ring buffer with the given capacity.
func NewLogBuffer(capacity int) *LogBuffer {
	return &LogBuffer{
		lines: make([]string, 0, capacity),
		cap:   capacity,
	}
}

// Write implements io.Writer. It splits input by newlines and appends each line.
func (lb *LogBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	text := string(p)
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		if len(lb.lines) >= lb.cap {
			lb.lines = lb.lines[1:]
		}
		lb.lines = append(lb.lines, line)
	}
	return len(p), nil
}

// ReadAll returns all buffered lines joined by newlines.
func (lb *LogBuffer) ReadAll() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return strings.Join(lb.lines, "\n")
}

// Clear removes all buffered lines.
func (lb *LogBuffer) Clear() {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.lines = lb.lines[:0]
}

// ReadAndClear atomically returns all buffered lines and clears the buffer.
func (lb *LogBuffer) ReadAndClear() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	result := strings.Join(lb.lines, "\n")
	lb.lines = lb.lines[:0]
	return result
}

// tunnelState holds the result of a connection attempt.
// Returned by connectDirect/connectRelay for applyState to install.
type tunnelState struct {
	mgr          *turn.Manager
	m            *mux.Mux
	conns        []io.ReadWriteCloser       // connections to add after idle timeout is set
	cleanups     []context.CancelFunc
	sigClient    provider.SignalingClient    // non-nil in relay mode; kept alive for reconnect signaling
	sessionID    uuid.UUID                  // session UUID for reconnect auth
	joinInfo     *provider.JoinInfo         // cached for fast reconnect
	pool         *tunnel.CallPool           // non-nil in multi-call pool mode
	tmReconnect  *telemost.ReconnectManager // non-nil in Telemost mode; per-index reconnect
}

// Tunnel is the main gomobile-exported type for mobile platforms.
type Tunnel struct {
	mu         sync.Mutex
	mgr        *turn.Manager
	m          *mux.Mux
	logger     *slog.Logger
	logBuf     *LogBuffer
	cleanups   []context.CancelFunc
	nextStream atomic.Uint32
	running    bool

	// reconnect infrastructure
	cfg        *TunnelConfig
	fpBytes    []byte          // parsed fingerprint bytes from cfg.Fingerprint
	svc        provider.Service
	rootCtx    context.Context
	rootCancel context.CancelFunc
	muxReady   chan struct{}           // closed when mux is available; recreated on teardown
	muxCancel  context.CancelFunc     // cancels DispatchLoop/PingLoop for current mux
	connectedAt time.Time             // when the current MUX was established
	sigClient  provider.SignalingClient // kept alive in relay mode for per-conn reconnects
	sessionID  uuid.UUID              // current session ID

	// credentials cache — reused on reconnect to skip VK API calls
	cachedJoinInfo *provider.JoinInfo
	cachedCreds    []*provider.Credentials

	// network change debouncing
	networkDebounce *time.Timer    // coalesces rapid network change events
	networkForce    chan struct{}   // signals reconnectLoop to do immediate full reconnect

	// hot-reload scripts manager (VK config, captcha scripts) — lifecycle tied to Tunnel
	scripts       *scripts.Manager
	scriptsCtx    context.Context
	scriptsCancel context.CancelFunc

	// multi-call pool (non-nil in pool mode)
	pool *tunnel.CallPool

	// connection stage (for UI display)
	stage atomic.Value // string

	// fatal error — set when reconnect gives up permanently
	fatalErr atomic.Value // string

	// captcha callback (set via SetCaptchaCallback before Start)
	captchaCb CaptchaCallback
}

// MaxRecommendedConns is the maximum number of parallel connections
// recommended for stable operation. Exceeding this may cause VK call
// instability and potential call blocking.
const MaxRecommendedConns = 8

// TunnelConfig holds configuration for starting the tunnel.
type TunnelConfig struct {
	CallLink    string // call-link ID
	ServerAddr  string // VPN server address (host:port), empty = relay-to-relay mode
	NumConns    int    // parallel TURN+DTLS connections
	UseTCP      bool   // TCP vs UDP for TURN
	Token       string // auth token for server (empty = no auth)
	Fingerprint string // server DTLS certificate SHA-256 fingerprint (hex, empty = no pinning)
	VKTokens    string // comma-separated VK tokens (0-16), passed from Android UI
}

// ValidateNumConns returns a warning message if NumConns exceeds the
// recommended maximum, or an empty string if the value is safe.
// Mobile apps should call this and display the warning under the input field.
func ValidateNumConns(n int) string {
	if n > MaxRecommendedConns {
		return fmt.Sprintf("Warning: %d connections exceeds the recommended maximum of %d. "+
			"This may cause call instability and potential VK call blocking.", n, MaxRecommendedConns)
	}
	return ""
}

// NewTunnel creates a new tunnel instance.
func NewTunnel() *Tunnel {
	lb := NewLogBuffer(500)
	return &Tunnel{
		logBuf: lb,
		logger: slog.New(slog.NewTextHandler(io.MultiWriter(lb, os.Stderr), &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// SetCaptchaCallback sets the callback for solving VK captchas via WebView.
// Must be called before Start(). If not set, captcha errors are propagated.
func (t *Tunnel) SetCaptchaCallback(cb CaptchaCallback) {
	t.captchaCb = cb
}

// InitScripts initialises the hot-reload scripts manager. Call once at app
// start, before Start(). All parameters are optional: if url is empty the
// manager relies on compile-time DefaultURL and bundled fallback; if localDir
// is empty a writable cache under the process temp dir is used. Must be
// balanced by ShutdownScripts() if you wish to stop background polling early;
// otherwise Stop() will clean up.
func (t *Tunnel) InitScripts(url, pubKey, localDir string) {
	if t.scripts != nil {
		return
	}
	cfg := scripts.ResolveConfig(scripts.Config{
		URL:       url,
		PublicKey: pubKey,
		LocalDir:  localDir,
		Logger:    scripts.NewSlogLogger(t.logger.With("component", "scripts")),
	})
	if cfg.LocalDir == "" {
		cfg.LocalDir = scripts.DefaultLocalDir()
	}
	t.scripts = scripts.NewManager(cfg)
	t.scriptsCtx, t.scriptsCancel = context.WithCancel(context.Background())
	if err := t.scripts.Start(t.scriptsCtx); err != nil {
		t.logger.Warn("scripts manager start failed", "err", err)
	}
	scriptshook.Register(t.scripts)
}

// TriggerScriptsCheck requests an immediate manifest check. Safe to call when
// InitScripts has not been invoked (no-op).
func (t *Tunnel) TriggerScriptsCheck() {
	if t.scripts != nil {
		t.scripts.TriggerCheck()
	}
}

// ScriptsVersion returns the version of the currently loaded bundle, or an
// empty string if no bundle is loaded.
func (t *Tunnel) ScriptsVersion() string {
	if t.scripts == nil {
		return ""
	}
	b := t.scripts.Current()
	if b == nil || b.Manifest == nil {
		return ""
	}
	return b.Manifest.Version
}

// ShutdownScripts stops the background polling loop. Idempotent.
func (t *Tunnel) ShutdownScripts() {
	if t.scriptsCancel != nil {
		t.scriptsCancel()
		t.scriptsCancel = nil
	}
	if t.scripts != nil {
		t.scripts.Stop()
		scriptshook.Register(nil)
		t.scripts = nil
	}
}

// APKUpdateVersion returns the APK version advertised in the current
// manifest, or empty string if no APK entry is present.
func (t *Tunnel) APKUpdateVersion() string {
	if t.scripts == nil {
		return ""
	}
	b := t.scripts.Current()
	if b == nil || b.Manifest == nil || b.Manifest.APK == nil {
		return ""
	}
	return b.Manifest.APK.Version
}

// APKUpdateURL returns the download URL for the APK in the current manifest,
// or empty string if no APK entry is present.
func (t *Tunnel) APKUpdateURL() string {
	if t.scripts == nil {
		return ""
	}
	b := t.scripts.Current()
	if b == nil || b.Manifest == nil || b.Manifest.APK == nil {
		return ""
	}
	return b.Manifest.APK.URL
}

// APKUpdateSHA256 returns the SHA-256 hex of the advertised APK, or empty.
func (t *Tunnel) APKUpdateSHA256() string {
	if t.scripts == nil {
		return ""
	}
	b := t.scripts.Current()
	if b == nil || b.Manifest == nil || b.Manifest.APK == nil {
		return ""
	}
	return b.Manifest.APK.SHA256
}

// DownloadAPK fetches the APK advertised in the current manifest, verifies
// its sha256, writes it to destPath, and returns the absolute path. Blocks
// for up to 10 minutes.
func (t *Tunnel) DownloadAPK(destPath string) error {
	if t.scripts == nil {
		return fmt.Errorf("scripts manager not initialised")
	}
	b := t.scripts.Current()
	if b == nil || b.Manifest == nil || b.Manifest.APK == nil {
		return fmt.Errorf("no APK entry in current manifest")
	}
	apk := b.Manifest.APK
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apk.URL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != apk.SHA256 {
		return fmt.Errorf("apk sha256 mismatch")
	}
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		return err
	}
	t.logger.Info("apk downloaded", "path", destPath, "version", apk.Version, "bytes", len(data))
	return nil
}

// ReadLogs returns all buffered log lines as a single string
// and atomically clears the buffer.
func (t *Tunnel) ReadLogs() string {
	return t.logBuf.ReadAndClear()
}

// ClearLogs removes all buffered log lines.
func (t *Tunnel) ClearLogs() {
	t.logBuf.Clear()
}

// Stage returns the current connection stage for UI display.
// Returns empty string when connected or idle.
func (t *Tunnel) Stage() string {
	v := t.stage.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

func (t *Tunnel) setStage(s string) {
	t.stage.Store(s)
	t.logger.Info("stage: " + s)
}

// FatalError returns the fatal error message if the tunnel has permanently
// failed (e.g. reconnect exhausted). Returns empty string while healthy.
func (t *Tunnel) FatalError() string {
	v := t.fatalErr.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

func (t *Tunnel) setFatalError(msg string) {
	t.fatalErr.Store(msg)
	t.logger.Error("fatal: " + msg)
}

// Start establishes TURN+DTLS connections and starts the mux tunnel.
// If ServerAddr is empty, uses relay-to-relay mode via signaling.
// A background reconnect loop monitors connection health and re-establishes
// the tunnel automatically when all DTLS connections die.
func (t *Tunnel) Start(cfg *TunnelConfig) error {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return fmt.Errorf("tunnel already running")
	}
	if cfg.NumConns <= 0 {
		cfg.NumConns = 16
	}
	if warn := ValidateNumConns(cfg.NumConns); warn != "" {
		t.logger.Warn(warn)
	}
	t.cfg = cfg
	if cfg.Fingerprint != "" {
		fp, err := hex.DecodeString(cfg.Fingerprint)
		if err != nil || len(fp) != 32 {
			t.mu.Unlock()
			return fmt.Errorf("invalid fingerprint: must be 64 hex characters (SHA-256)")
		}
		t.fpBytes = fp
	} else {
		t.fpBytes = nil
	}
	// Parse links: comma-separated for multi-call pool
	links := strings.Split(cfg.CallLink, ",")
	var cleanLinks []string
	for _, l := range links {
		l = strings.TrimSpace(l)
		if l != "" {
			cleanLinks = append(cleanLinks, l)
		}
	}
	if len(cleanLinks) == 0 {
		t.mu.Unlock()
		return fmt.Errorf("no valid call links provided")
	}

	if telemost.IsTelemostLink(cleanLinks[0]) {
		t.svc = telemost.NewService(cleanLinks[0], cfg.Token)
	} else {
		var vkOpts []vk.Option
		{
			solvers := []provider.CaptchaSolver{captcha.NewLuaSolver(t.scripts)}
			if t.captchaCb != nil {
				solvers = append(solvers, &callbackSolver{cb: t.captchaCb})
			}
			vkOpts = append(vkOpts, vk.WithCaptchaSolver(captcha.NewChainSolver(solvers...)))
		}
		t.svc = vk.NewService(cleanLinks[0], vkOpts...)
	}
	t.rootCtx, t.rootCancel = context.WithCancel(context.Background())
	t.muxReady = make(chan struct{})
	t.networkForce = make(chan struct{}, 1)
	t.running = true
	t.mu.Unlock()

	var state *tunnelState
	var err error
	if len(cleanLinks) > 1 && cfg.ServerAddr == "" {
		// Multi-call pool mode
		state, err = t.connectMultiRelay(t.rootCtx, cfg, cleanLinks)
	} else if cfg.ServerAddr != "" {
		state, err = t.connectDirect(t.rootCtx, cfg)
	} else if telemost.IsTelemostLink(cleanLinks[0]) {
		state, err = t.connectTelemost(t.rootCtx, cfg)
	} else {
		state, err = t.connectRelay(t.rootCtx, cfg)
	}
	if err != nil {
		t.rootCancel()
		t.mu.Lock()
		t.running = false
		t.mu.Unlock()
		return err
	}

	t.applyState(state)
	go t.reconnectLoop()
	return nil
}

// connectTelemost establishes Telemost WebRTC connections using paired display
// names derived from the auth token. Returns a tunnelState without mutating t.
func (t *Tunnel) connectTelemost(ctx context.Context, cfg *TunnelConfig) (*tunnelState, error) {
	t.mu.Lock()
	svc := t.svc
	t.mu.Unlock()

	tmSvc, ok := svc.(*telemost.Service)
	if !ok {
		return nil, fmt.Errorf("expected telemost.Service")
	}

	serverNames, clientNames := telemost.DeriveDisplayNames(cfg.Token, cfg.NumConns)
	sessionID := uuid.New()

	var muxConns []io.ReadWriteCloser
	var muxIndices []int
	var cleanups []context.CancelFunc
	for i := 0; i < cfg.NumConns; i++ {
		conn, cleanup, err := tmSvc.ConnectPaired(ctx,
			t.logger.With("index", i), clientNames[i], serverNames[i], i)
		if err != nil {
			t.logger.Warn("Telemost connection failed", "index", i, "err", err)
			continue
		}

		if cfg.Token != "" {
			if err := mux.WriteAuthToken(conn, cfg.Token); err != nil {
				t.logger.Warn("write auth token failed", "index", i, "err", err)
				cleanup()
				continue
			}
		}

		var sid [16]byte
		copy(sid[:], sessionID[:])
		if err := mux.WriteSessionID(conn, sid); err != nil {
			t.logger.Warn("write session id failed", "index", i, "err", err)
			cleanup()
			continue
		}

		cleanups = append(cleanups, cleanup)
		muxConns = append(muxConns, conn)
		muxIndices = append(muxIndices, i)
	}

	if len(muxConns) == 0 {
		for _, c := range cleanups {
			c()
		}
		return nil, fmt.Errorf("no Telemost connections established")
	}

	m := mux.New(t.logger)

	// ReconnectManager поднимает упавшие per-index соединения в фоне (applyState
	// запускает Run()). Оборачиваем первичные conn'ы, чтобы manager знал о падении.
	tmReconnect := telemost.NewReconnectManager(telemost.ReconnectConfig{
		Service:     tmSvc,
		TargetConns: cfg.NumConns,
		AuthToken:   cfg.Token,
		SessionID:   sessionID,
		ServerNames: serverNames,
		ClientNames: clientNames,
		Logger:      t.logger,
	}, m)
	muxConns = tmReconnect.WrapInitial(muxConns, muxIndices)

	t.logger.Info("tunnel connected (telemost)",
		"active", len(muxConns), "target", cfg.NumConns,
		"session_id", sessionID.String())
	return &tunnelState{
		m:           m,
		conns:       muxConns,
		cleanups:    cleanups,
		sessionID:   sessionID,
		tmReconnect: tmReconnect,
	}, nil
}

// connectDirect creates TURN allocations and DTLS connections to a server
// listening on a direct UDP address. Returns a tunnelState without mutating t.
func (t *Tunnel) connectDirect(ctx context.Context, cfg *TunnelConfig) (*tunnelState, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", cfg.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve server: %w", err)
	}

	sessionID := uuid.New()

	t.mu.Lock()
	svc := t.svc
	t.mu.Unlock()

	mgr := turn.NewManager(svc, cfg.UseTCP, t.logger)
	allocs, err := mgr.Allocate(ctx, cfg.NumConns)
	if err != nil {
		mgr.CloseAll()
		return nil, fmt.Errorf("allocate TURN: %w", err)
	}

	var muxConns []io.ReadWriteCloser
	var cleanups []context.CancelFunc
	for i, alloc := range allocs {
		dtlsConn, cleanup, err := internaldtls.DialOverTURN(ctx, alloc.RelayConn, serverAddr, t.fpBytes)
		if err != nil {
			t.logger.Warn("DTLS-over-TURN failed", "index", i, "err", err)
			continue
		}
		cleanups = append(cleanups, cleanup)

		if cfg.Token != "" {
			if err := mux.WriteAuthToken(dtlsConn, cfg.Token); err != nil {
				t.logger.Warn("write auth token failed", "index", i, "err", err)
				cleanup()
				continue
			}
		}

		var sid [16]byte
		copy(sid[:], sessionID[:])
		if err := mux.WriteSessionID(dtlsConn, sid); err != nil {
			t.logger.Warn("write session id failed", "index", i, "err", err)
			cleanup()
			continue
		}

		muxConns = append(muxConns, dtlsConn)
	}

	if len(muxConns) == 0 {
		mgr.CloseAll()
		for _, c := range cleanups {
			c()
		}
		return nil, fmt.Errorf("no DTLS connections established")
	}

	m := mux.New(t.logger)
	t.logger.Info("tunnel connected (direct)",
		"active", len(muxConns), "target", cfg.NumConns,
		"session_id", sessionID.String())
	return &tunnelState{mgr: mgr, m: m, conns: muxConns, cleanups: cleanups, sessionID: sessionID}, nil
}

// deduplicateTokens removes duplicate VK tokens, logging a warning for each duplicate.
func deduplicateTokens(tokens []string, logger *slog.Logger) []string {
	seen := make(map[string]bool, len(tokens))
	result := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if seen[t] {
			logger.Warn("duplicate VK token ignored")
			continue
		}
		seen[t] = true
		result = append(result, t)
	}
	return result
}

// allocateWithToken fetches fresh TURN credentials using a VK token and allocates a relay.
func allocateWithToken(ctx context.Context, svc provider.Service, mgr *turn.Manager, token string) (*turn.Allocation, error) {
	tap, ok := svc.(provider.TokenAuthProvider)
	if !ok {
		return nil, fmt.Errorf("provider does not support token auth")
	}
	info, err := tap.FetchJoinInfoWithToken(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("fetch join info with token: %w", err)
	}
	alloc, err := mgr.AllocateWithCredentials(ctx, &info.Credentials)
	if err != nil {
		return nil, fmt.Errorf("allocate with credentials: %w", err)
	}
	return alloc, nil
}

// connectRelay creates TURN allocations, exchanges relay addresses via
// signaling, and establishes DTLS connections to the server's relay addresses.
// Returns a tunnelState without mutating t.
// connectMultiRelay establishes multiple VK calls feeding one shared MUX via CallPool.
func (t *Tunnel) connectMultiRelay(ctx context.Context, cfg *TunnelConfig, links []string) (*tunnelState, error) {
	var vkTokens []string
	if cfg.VKTokens != "" {
		vkTokens = strings.Split(cfg.VKTokens, ",")
	}

	services := make([]provider.Service, len(links))
	for i, link := range links {
		if telemost.IsTelemostLink(link) {
			services[i] = telemost.NewService(link, cfg.Token)
		} else {
			services[i] = vk.NewService(link)
		}
	}

	pool := tunnel.NewCallPool(tunnel.PoolConfig{
		Services:     services,
		ConnsPerCall: cfg.NumConns,
		UseTCP:       cfg.UseTCP,
		AuthToken:    cfg.Token,
		VKTokens:     vkTokens,
		Logger:       t.logger,
	})

	m, err := pool.StartClient(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("multi-relay pool start: %w", err)
	}

	go m.DispatchLoop(ctx)
	go m.StartPingLoop(ctx, 10*time.Second)

	t.logger.Info("multi-relay tunnel connected", "calls", len(links), "active", m.ActiveConns())

	return &tunnelState{
		m:    m,
		pool: pool,
	}, nil
}

func (t *Tunnel) connectRelay(ctx context.Context, cfg *TunnelConfig) (*tunnelState, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("token is required for relay-to-relay mode")
	}

	t.mu.Lock()
	svc := t.svc
	cachedJI := t.cachedJoinInfo
	cachedCreds := t.cachedCreds
	t.mu.Unlock()

	// Parse VK tokens from comma-separated string.
	var vkTokens []string
	if cfg.VKTokens != "" {
		vkTokens = strings.Split(cfg.VKTokens, ",")
	}

	// Deduplicate and cap to numConns.
	vkTokens = deduplicateTokens(vkTokens, t.logger)
	if len(vkTokens) > cfg.NumConns {
		vkTokens = vkTokens[:cfg.NumConns]
	}

	// Reuse cached JoinInfo from previous session if available.
	// If VK tokens are present, prefer authenticated join for fresh JoinInfo.
	var jr *provider.JoinInfo
	if cachedJI != nil {
		t.logger.Info("reusing cached JoinInfo", "conv_id", cachedJI.ConvID)
		jr = cachedJI
	} else {
		t.setStage("Получение токена VK...")
		var err error
		// Always use anonymous flow for signaling — VK kicks authenticated
		// participants faster than anonymous ones.
		jr, err = svc.FetchJoinInfo(ctx)
		if err != nil {
			return nil, fmt.Errorf("join conference: %w", err)
		}
		t.logger.Info("joined conference", "conv_id", jr.ConvID)
	}

	// Set up signaling (connect + SetKey + disconnect handshake).
	t.setStage("Подключение к сигналингу...")
	sigClient, err := svc.ConnectSignaling(ctx, jr, t.logger.With("component", "signaling"))
	if err != nil {
		// If we used cached JoinInfo and signaling failed, retry with fresh fetch.
		if cachedJI != nil {
			t.logger.Warn("cached JoinInfo stale, fetching fresh", "err", err)
			t.mu.Lock()
			t.cachedJoinInfo = nil
			t.cachedCreds = nil
			t.mu.Unlock()
			return t.connectRelay(ctx, cfg)
		}
		return nil, fmt.Errorf("signaling connect: %w", err)
	}
	if err := sigClient.SetKey(cfg.Token); err != nil {
		sigClient.Close()
		return nil, fmt.Errorf("set signaling key: %w", err)
	}

	// Send accept-call immediately to prevent VK idle kick.
	// Must be before disconnect handshake (which takes up to 1s) — authenticated
	// participants get kicked after ~7s without accept-call.
	if ac, ok := sigClient.(interface{ SendAcceptCall() error }); ok {
		if err := ac.SendAcceptCall(); err != nil {
			t.logger.Warn("failed to send accept-call", "err", err)
		}
	}

	// Generate session nonce for filtering ghost messages.
	nonceBytes := make([]byte, 8)
	rand.Read(nonceBytes)
	nonce := hex.EncodeToString(nonceBytes)

	t.setStage("Сброс старой сессии...")
	// Tell server to kill any existing session — fire once with a short timeout.
	_ = sigClient.SendDisconnectReq(ctx, nonce)
	ackCtx, ackCancel := context.WithTimeout(ctx, 1*time.Second)
	if err := sigClient.WaitDisconnectAck(ackCtx, nonce); err == nil {
		t.logger.Info("disconnect ack received", "nonce", nonce)
	}
	ackCancel()

	// Start batched TURN allocations with cached credentials for anonymous path.
	t.setStage("Подключение к TURN...")
	var credsProvider provider.CredentialsProvider = svc
	if len(cachedCreds) > 0 {
		t.logger.Info("reusing cached TURN credentials", "count", len(cachedCreds))
		credsProvider = turn.NewCachedProvider(cachedCreds, svc)
	}
	mgr := turn.NewManager(credsProvider, cfg.UseTCP, t.logger)
	mgr.SetInitialCredentials(&jr.Credentials)

	sessionID := uuid.New()
	var (
		m        *mux.Mux
		muxConns []io.ReadWriteCloser
		cleanups []context.CancelFunc
		batchIdx int
	)

	type dtlsResult struct {
		conn    io.ReadWriteCloser
		cleanup context.CancelFunc
		err     error
	}

	// dtlsHandshake performs DTLS handshake over a relay connection, writes auth token
	// and session ID, and sends the result to the provided channel.
	dtlsHandshake := func(relayConn net.PacketConn, addr *net.UDPAddr, results chan<- dtlsResult, punchCtx context.Context) {
		var dtlsConn io.ReadWriteCloser
		var cleanup context.CancelFunc
		var lastErr error
		for attempt := 1; attempt <= 2; attempt++ {
			punchLoopCtx, punchLoopCancel := context.WithCancel(punchCtx)
			internaldtls.PunchRelay(relayConn, addr)
			go internaldtls.StartPunchLoop(punchLoopCtx, relayConn, addr)
			time.Sleep(200 * time.Millisecond) // let TURN permissions establish

			var c net.Conn
			c, cleanup, lastErr = internaldtls.DialOverTURN(ctx, relayConn, addr, t.fpBytes)
			punchLoopCancel()

			if lastErr == nil {
				dtlsConn = c
				break
			}
			t.logger.Warn("DTLS handshake failed", "attempt", attempt, "err", lastErr)
			if attempt < 2 {
				time.Sleep(time.Duration(attempt) * time.Second)
			}
		}
		if lastErr != nil {
			results <- dtlsResult{err: lastErr}
			return
		}

		if cfg.Token != "" {
			if err := mux.WriteAuthToken(dtlsConn, cfg.Token); err != nil {
				cleanup()
				results <- dtlsResult{err: fmt.Errorf("write auth token: %w", err)}
				return
			}
		}

		var sid [16]byte
		copy(sid[:], sessionID[:])
		if err := mux.WriteSessionID(dtlsConn, sid); err != nil {
			cleanup()
			results <- dtlsResult{err: fmt.Errorf("write session id: %w", err)}
			return
		}

		results <- dtlsResult{conn: dtlsConn, cleanup: cleanup}
	}

	// collectDTLS collects results from the channel and adds successful connections to the MUX.
	collectDTLS := func(results <-chan dtlsResult, count int) {
		for j := 0; j < count; j++ {
			r := <-results
			if r.err != nil {
				t.logger.Warn("relay DTLS failed in batch", "batch", batchIdx, "err", r.err)
				continue
			}
			cleanups = append(cleanups, r.cleanup)
			if m == nil {
				m = mux.New(t.logger)
				t.logger.Info("first relay connection ready, MUX created", "batch", batchIdx)
			}
			muxConns = append(muxConns, r.conn)
		}
	}

	// --- Token allocation phase (batch 0) ---
	tokenConns := len(vkTokens)
	anonConns := cfg.NumConns - tokenConns

	if tokenConns > 0 {
		t.setStage("Обмен relay адресами (токены)...")
		tokenAllocs := make([]*turn.Allocation, tokenConns)
		var tokenWg sync.WaitGroup
		var tokenMu sync.Mutex
		for i, tok := range vkTokens {
			tokenWg.Add(1)
			go func(idx int, token string) {
				defer tokenWg.Done()
				var alloc *turn.Allocation
				var aErr error
				if idx == 0 {
					// First token was already used for initial join — reuse those credentials.
					alloc, aErr = mgr.AllocateWithCredentials(ctx, &jr.Credentials)
				} else {
					alloc, aErr = allocateWithToken(ctx, svc, mgr, token)
				}
				tokenMu.Lock()
				defer tokenMu.Unlock()
				if aErr != nil {
					t.logger.Warn("token allocation failed", "index", idx, "err", aErr)
					return
				}
				tokenAllocs[idx] = alloc
			}(i, tok)
		}
		tokenWg.Wait()

		// Collect successful allocations.
		var tokenAddrs []string
		var successAllocs []*turn.Allocation
		for _, a := range tokenAllocs {
			if a != nil {
				tokenAddrs = append(tokenAddrs, a.RelayAddr.String())
				successAllocs = append(successAllocs, a)
			}
		}

		if len(tokenAddrs) > 0 {
			final := anonConns == 0
			if err := sigClient.SendRelayBatch(ctx, tokenAddrs, "client", nonce, batchIdx, final); err != nil {
				mgr.CloseAll()
				sigClient.Close()
				return nil, fmt.Errorf("send token relay batch: %w", err)
			}
			serverAddrs, _, _, _, err := sigClient.RecvRelayBatch(ctx, "client", nonce)
			if err != nil {
				mgr.CloseAll()
				sigClient.Close()
				return nil, fmt.Errorf("recv token relay batch: %w", err)
			}

			pairCount := len(successAllocs)
			if len(serverAddrs) < pairCount {
				pairCount = len(serverAddrs)
			}

			if pairCount > 0 {
				if batchIdx == 0 {
					t.setStage("Установка DTLS...")
				}
				results := make(chan dtlsResult, pairCount)
				punchCtx, punchCancel := context.WithCancel(ctx)
				for i := 0; i < pairCount; i++ {
					serverUDP, rErr := net.ResolveUDPAddr("udp", serverAddrs[i])
					if rErr != nil {
						results <- dtlsResult{err: rErr}
						continue
					}
					go dtlsHandshake(successAllocs[i].RelayConn, serverUDP, results, punchCtx)
				}
				collectDTLS(results, pairCount)
				punchCancel()
			}
		}

		// Failed tokens fall back to anonymous allocations.
		failedTokens := tokenConns - len(successAllocs)
		anonConns += failedTokens
		batchIdx++
	}

	// --- Anonymous allocation phase ---
	t.setStage("Обмен relay адресами...")
	if anonConns > 0 {
		batchCh := mgr.AllocateGradual(ctx, anonConns, turn.GradualOpts{})

		for br := range batchCh {
			if len(br.Allocs) == 0 {
				if br.Final {
					break
				}
				continue
			}

			addrs := make([]string, len(br.Allocs))
			for i, a := range br.Allocs {
				addrs[i] = a.RelayAddr.String()
			}
			t.logger.Info("batch ready", "batch", batchIdx, "allocs", len(br.Allocs), "final", br.Final)

			if err := sigClient.SendRelayBatch(ctx, addrs, "client", nonce, batchIdx, br.Final); err != nil {
				t.logger.Error("send relay batch", "batch", batchIdx, "err", err)
				break
			}
			serverAddrs, _, _, _, err := sigClient.RecvRelayBatch(ctx, "client", nonce)
			if err != nil {
				t.logger.Error("recv relay batch", "batch", batchIdx, "err", err)
				break
			}

			pairCount := len(br.Allocs)
			if len(serverAddrs) < pairCount {
				pairCount = len(serverAddrs)
			}
			if pairCount == 0 {
				batchIdx++
				if br.Final {
					break
				}
				continue
			}

			if batchIdx == 0 {
				t.setStage("Установка DTLS...")
			}

			results := make(chan dtlsResult, pairCount)
			punchCtx, punchCancel := context.WithCancel(ctx)

			for i := 0; i < pairCount; i++ {
				serverUDP, err := net.ResolveUDPAddr("udp", serverAddrs[i])
				if err != nil {
					results <- dtlsResult{err: err}
					continue
				}
				go dtlsHandshake(br.Allocs[i].RelayConn, serverUDP, results, punchCtx)
			}

			collectDTLS(results, pairCount)
			punchCancel()

			batchIdx++
			if br.Final {
				break
			}
		}
	}

	if m == nil {
		sigClient.Close()
		mgr.CloseAll()
		for _, c := range cleanups {
			c()
		}
		return nil, fmt.Errorf("no relay DTLS connections established")
	}

	t.logger.Info("tunnel connected (relay-to-relay)",
		"active", len(muxConns), "target", cfg.NumConns,
		"session_id", sessionID.String())
	return &tunnelState{mgr: mgr, m: m, conns: muxConns, cleanups: cleanups, sigClient: sigClient, sessionID: sessionID, joinInfo: jr}, nil
}

// applyState installs a new tunnelState into the tunnel, starting
// DispatchLoop and PingLoop, and signals muxReady.
func (t *Tunnel) applyState(state *tunnelState) {
	t.mu.Lock()
	t.m = state.m
	t.mgr = state.mgr
	t.cleanups = state.cleanups
	t.sigClient = state.sigClient
	t.sessionID = state.sessionID
	t.pool = state.pool
	t.connectedAt = time.Now()
	if state.joinInfo != nil {
		t.cachedJoinInfo = state.joinInfo
	}
	muxCtx, muxCancel := context.WithCancel(t.rootCtx)
	t.muxCancel = muxCancel
	ready := t.muxReady
	t.mu.Unlock()

	t.setStage("") // clear stage — connected
	state.m.EnableRawPackets(4096)
	state.m.SetIdleTimeout(15 * time.Second)
	go state.m.DispatchLoop(muxCtx)
	go state.m.StartPingLoop(muxCtx, 5*time.Second)
	if state.mgr != nil {
		go state.mgr.StartKeepalive(muxCtx, 10*time.Second)
	}

	// Add connections AFTER idle timeout and DispatchLoop are set up.
	for _, conn := range state.conns {
		state.m.AddConn(conn)
	}

	// Start per-connection reconnect for Telemost mode.
	if state.tmReconnect != nil {
		go state.tmReconnect.Run(muxCtx)
	}

	// Start per-connection reconnect for relay mode.
	if state.sigClient != nil {
		go client.NewReconnectManager(client.ReconnectConfig{
			TargetConns:     t.cfg.NumConns,
			AuthToken:       t.cfg.Token,
			Fingerprint:     t.fpBytes,
			SessionID:       state.sessionID,
			Logger:          t.logger,
			OnFullReconnect: func() { t.teardownMux() },
		}, state.sigClient, state.mgr, state.m).Run(muxCtx)
		go func() {
			reason, _ := state.sigClient.WaitForSessionEnd(muxCtx)
			if reason == provider.SessionEndHungup {
				t.logger.Warn("VK terminated the call (hungup), triggering full session reconnect")
				t.teardownMux()
			}
		}()
	}

	close(ready)
}

// teardownMux idempotently tears down the current mux, TURN manager, and
// all DTLS cleanups. A new muxReady channel is created for the next connection.
func (t *Tunnel) teardownMux() {
	t.mu.Lock()
	muxCancel := t.muxCancel
	m := t.m
	cleanups := t.cleanups
	mgr := t.mgr
	sig := t.sigClient

	// Cache credentials from live allocations for fast reconnect.
	if mgr != nil {
		if creds := mgr.AllCredentials(); len(creds) > 0 {
			t.cachedCreds = creds
		}
	}

	pool := t.pool

	t.muxCancel = nil
	t.m = nil
	t.cleanups = nil
	t.mgr = nil
	t.sigClient = nil
	t.pool = nil
	// Only create a new muxReady if there was something to tear down.
	// Repeated calls with nothing to tear down must not replace the channel
	// (getMux may already be waiting on it).
	if m != nil || muxCancel != nil {
		t.muxReady = make(chan struct{})
	}
	t.mu.Unlock()

	// Close signaling FIRST — while the WebSocket may still be alive,
	// send hangup to remove our participant from the VK call.
	// This prevents ghost participants when the network is degraded
	// but not yet fully dead.
	if sig != nil {
		sig.Close()
	}
	if muxCancel != nil {
		muxCancel()
	}
	if m != nil {
		m.Close()
	}
	for _, c := range cleanups {
		c()
	}
	if mgr != nil {
		mgr.CloseAll()
	}
	if pool != nil {
		pool.Close()
	}
}

// reconnectLoop watches the current mux for death (or network change signal)
// and re-establishes the tunnel with exponential backoff (1s → 60s).
func (t *Tunnel) reconnectLoop() {
	const maxBackoff = 60 * time.Second
	const attemptTimeout = 20 * time.Second
	const maxConsecutiveFailures = 10 // give up after ~5 min of failures
	backoff := time.Second

	for {
		t.mu.Lock()
		m := t.m
		force := t.networkForce
		t.mu.Unlock()

		if m == nil {
			select {
			case <-t.rootCtx.Done():
				return
			case <-force:
			case <-time.After(backoff):
			}
		} else {
			select {
			case <-m.Dead():
				t.logger.Info("all connections dead, starting reconnect")
				t.setStage("Переподключение...")
				t.teardownMux()
			case <-force:
				t.logger.Info("network change forced reconnect")
				t.setStage("Переподключение...")
				t.teardownMux()
			case <-t.rootCtx.Done():
				return
			}
		}

		// Drain any pending force signal so it doesn't trigger again.
		select {
		case <-force:
		default:
		}

		firstAttempt := true
		consecutiveFailures := 0
		for {
			if !firstAttempt {
				select {
				case <-t.rootCtx.Done():
					return
				case <-time.After(backoff):
				}
			}
			firstAttempt = false

			attemptCtx, attemptCancel := context.WithTimeout(t.rootCtx, attemptTimeout)
			var state *tunnelState
			var err error
			if t.cfg.ServerAddr != "" {
				state, err = t.connectDirect(attemptCtx, t.cfg)
			} else if telemost.IsTelemostLink(t.cfg.CallLink) {
				state, err = t.connectTelemost(attemptCtx, t.cfg)
			} else {
				state, err = t.connectRelay(attemptCtx, t.cfg)
			}
			attemptCancel()

			if err != nil {
				consecutiveFailures++
				t.logger.Warn("reconnect attempt failed", "err", err, "attempt", consecutiveFailures, "backoff", backoff)

				if consecutiveFailures >= maxConsecutiveFailures {
					t.setFatalError(fmt.Sprintf("reconnect failed after %d attempts: %v", consecutiveFailures, err))
					t.teardownMux()
					return
				}

				backoff = min(backoff*2, maxBackoff)
				select {
				case <-force:
					t.logger.Info("network changed during reconnect, resetting backoff")
					backoff = time.Second
					consecutiveFailures = 0
				default:
				}
				continue
			}

			t.applyState(state)
			t.logger.Info("reconnected successfully")
			backoff = time.Second
			select {
			case <-force:
			default:
			}
			break
		}
	}
}

// getMux returns the current mux, blocking until one is available
// if a reconnect is in progress.
func (t *Tunnel) getMux() (*mux.Mux, error) {
	t.mu.Lock()
	m := t.m
	ready := t.muxReady
	t.mu.Unlock()

	if m != nil {
		return m, nil
	}

	select {
	case <-ready:
	case <-t.rootCtx.Done():
		return nil, fmt.Errorf("tunnel stopped")
	}

	t.mu.Lock()
	m = t.m
	t.mu.Unlock()
	if m == nil {
		return nil, fmt.Errorf("tunnel not available")
	}
	return m, nil
}

// DialStream opens a new mux stream to the given target address (host:port).
func (t *Tunnel) DialStream(addr string) (io.ReadWriteCloser, error) {
	m, err := t.getMux()
	if err != nil {
		return nil, err
	}

	id := t.nextStream.Add(1)
	stream, err := m.OpenStream(id)
	if err != nil {
		return nil, err
	}
	if _, err := stream.Write([]byte(addr)); err != nil {
		stream.Close()
		return nil, err
	}
	return stream, nil
}

// TunMTU is the recommended MTU for the native TUN device.
const TunMTU = 1280

// WritePacket sends a raw IP packet through the tunnel.
func (t *Tunnel) WritePacket(data []byte) error {
	t.mu.Lock()
	m := t.m
	t.mu.Unlock()

	if m == nil {
		return fmt.Errorf("tunnel reconnecting")
	}

	if len(data) > TunMTU {
		return nil // drop oversized packet
	}

	return m.SendRawPacket(&mux.Frame{
		StreamID: 0,
		Type:     mux.FrameData,
		Sequence: m.NextSeq(),
		Length:   uint32(len(data)),
		Payload:  data,
	})
}

// ReadPacket reads a raw IP packet from the tunnel.
func (t *Tunnel) ReadPacket(buf []byte) (int, error) {
	for {
		m, err := t.getMux()
		if err != nil {
			return 0, err
		}

		rb := m.RawPackets()
		if rb == nil {
			return 0, fmt.Errorf("raw packet mode not enabled")
		}

		// Try to pop a frame without blocking first.
		if f, ok := rb.Pop(); ok {
			if len(f.Payload) == 0 {
				continue
			}
			n := copy(buf, f.Payload)
			return n, nil
		}

		// Wait for data or shutdown.
		select {
		case _, ok := <-rb.Ready():
			if !ok {
				// Ring buffer closed (mux died). Retry with new mux.
				select {
				case <-t.rootCtx.Done():
					return 0, fmt.Errorf("tunnel stopped")
				case <-time.After(100 * time.Millisecond):
					continue
				}
			}
			// Signaled — pop again.
			if f, ok := rb.Pop(); ok {
				if len(f.Payload) == 0 {
					continue
				}
				n := copy(buf, f.Payload)
				return n, nil
			}
		case <-t.rootCtx.Done():
			return 0, fmt.Errorf("tunnel stopped")
		}
	}
}

// ReadPacketData reads a raw IP packet and returns it as a new byte slice.
func (t *Tunnel) ReadPacketData() ([]byte, error) {
	buf := make([]byte, TunMTU)
	n, err := t.ReadPacket(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// IsConnected reports whether the tunnel currently has an active mux.
func (t *Tunnel) IsConnected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m != nil
}

// ActiveConns returns the number of active DTLS connections in the mux.
func (t *Tunnel) ActiveConns() int {
	t.mu.Lock()
	m := t.m
	t.mu.Unlock()
	if m == nil {
		return 0
	}
	return m.ActiveConns()
}

// TotalConns returns the target number of connections (from config).
func (t *Tunnel) TotalConns() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cfg == nil {
		return 0
	}
	return int(t.cfg.NumConns)
}

// Stop tears down all connections and stops the reconnect loop.
func (t *Tunnel) Stop() {
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return
	}
	t.running = false
	rootCancel := t.rootCancel
	if t.networkDebounce != nil {
		t.networkDebounce.Stop()
		t.networkDebounce = nil
	}
	t.mu.Unlock()

	if rootCancel != nil {
		rootCancel()
	}
	t.teardownMux()
	t.logger.Info("tunnel stopped")
}

// OnNetworkChanged should be called by the mobile platform when the network
// connectivity changes (e.g. WiFi→cellular, tower handoff).
//
// Uses a three-level strategy to minimize unnecessary teardowns:
//   Level 0 — MUX still healthy after settling: do nothing (tower handoff).
//   Level 1 — MUX unhealthy but signaling alive: probe connections so
//             ReconnectManager replaces dead ones (brief signal loss).
//   Level 2 — everything dead: full teardown + reconnect (WiFi↔cellular).
func (t *Tunnel) OnNetworkChanged() {
	const gracePeriod = 10 * time.Second  // ignore events right after connect
	const settleDelay = 1500 * time.Millisecond // wait for network to settle
	const healthProbe = 3 * time.Second   // MUX ping timeout after settling

	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return
	}

	connAge := time.Since(t.connectedAt)
	if connAge < gracePeriod {
		t.mu.Unlock()
		t.logger.Info("ignoring network change during grace period", "conn_age", connAge)
		return
	}

	// Debounce: cancel any pending check from a previous event.
	if t.networkDebounce != nil {
		t.networkDebounce.Stop()
	}
	t.networkDebounce = time.AfterFunc(settleDelay, func() {
		t.networkSettled(healthProbe)
	})
	t.mu.Unlock()
}

// networkSettled runs after the settle delay. It determines the reconnect
// level and takes the minimum action needed.
func (t *Tunnel) networkSettled(healthProbe time.Duration) {
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return
	}
	m := t.m
	sig := t.sigClient
	force := t.networkForce
	t.mu.Unlock()

	// No active tunnel — nothing to check.
	if m == nil {
		return
	}

	// Level 0: MUX still healthy — tower handoff, connections survived.
	if m.IsHealthy(healthProbe) {
		t.logger.Info("network change: MUX still healthy, no action needed (level 0)")
		return
	}

	// MUX unhealthy — check signaling.
	if sig != nil && sig.IsAlive() {
		// Level 1: signaling alive — probe connections so ReconnectManager
		// detects dead ones and replaces them individually.
		t.logger.Info("network change: MUX unhealthy but signaling alive, probing connections (level 1)")
		m.ProbeConnections(3 * time.Second)
		return
	}

	// Level 2: everything dead — full teardown.
	t.logger.Info("network change: full teardown (level 2)", "sig_alive", sig != nil && sig.IsAlive())

	// Drain stale packets before teardown.
	if rb := m.RawPackets(); rb != nil {
		if n := rb.Drain(); n > 0 {
			t.logger.Info("drained stale raw packets on network change", "count", n)
		}
	}

	t.teardownMux()

	// Signal reconnect loop (non-blocking).
	select {
	case force <- struct{}{}:
	default:
	}
}

// IsRunning returns whether the tunnel is active.
func (t *Tunnel) IsRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

// SpeedTestCallback receives streaming speed test updates.
// Implemented by the Android/iOS app.
type SpeedTestCallback interface {
	OnPhase(phase string)
	OnProgress(jsonData string)
	OnComplete(jsonData string)
	OnError(err string)
}

// RunSpeedTest runs a speed test on the live tunnel.
// Results are streamed via the callback.
func (t *Tunnel) RunSpeedTest(cb SpeedTestCallback) {
	t.mu.Lock()
	m := t.m
	t.mu.Unlock()

	if m == nil {
		cb.OnError("tunnel not connected")
		return
	}

	adapter := &mobileCallbackAdapter{cb: cb}
	if err := speedtest.RunClient(m, adapter, t.logger); err != nil {
		cb.OnError(err.Error())
	}
}

type mobileCallbackAdapter struct {
	cb SpeedTestCallback
}

func (a *mobileCallbackAdapter) OnPhase(phase string)       { a.cb.OnPhase(phase) }
func (a *mobileCallbackAdapter) OnProgress(jsonData string) { a.cb.OnProgress(jsonData) }
func (a *mobileCallbackAdapter) OnComplete(jsonData string) { a.cb.OnComplete(jsonData) }
func (a *mobileCallbackAdapter) OnError(err string)         { a.cb.OnError(err) }
