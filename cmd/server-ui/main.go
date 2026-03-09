package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/telemost"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
	"github.com/call-vpn/call-vpn/internal/server"
)

//go:embed index.html
var indexHTML []byte

var linkRegex = regexp.MustCompile(`vk\.com/call/join/([A-Za-z0-9_-]+)`)

func parseCallLink(input string) string {
	m := linkRegex.FindStringSubmatch(input)
	if len(m) > 1 {
		return m[1]
	}
	return strings.TrimSpace(input)
}

// logWriter captures slog output and broadcasts to SSE clients.
type logWriter struct {
	mu      sync.Mutex
	lines   []string
	clients map[chan string]struct{}
}

func newLogWriter() *logWriter {
	return &logWriter{clients: make(map[chan string]struct{})}
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	text := strings.TrimRight(string(p), "\n")
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		w.lines = append(w.lines, line)
		if len(w.lines) > 5000 {
			w.lines = w.lines[len(w.lines)-5000:]
		}
		for ch := range w.clients {
			select {
			case ch <- line:
			default:
			}
		}
	}
	return len(p), nil
}

func (w *logWriter) subscribe() (chan string, func()) {
	ch := make(chan string, 64)
	w.mu.Lock()
	w.clients[ch] = struct{}{}
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		delete(w.clients, ch)
		w.mu.Unlock()
		close(ch)
	}
}

func (w *logWriter) allLines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.lines))
	copy(out, w.lines)
	return out
}

// instanceConfig is per-instance persistent config.
type instanceConfig struct {
	CallLink  string `json:"call_link"`
	AuthToken string `json:"auth_token"`
}

// appConfig persists between restarts.
type appConfig struct {
	Instances []instanceConfig `json:"instances"`
}

// legacyConfig is the old single-instance format.
type legacyConfig struct {
	CallLink  string `json:"call_link"`
	AuthToken string `json:"auth_token"`
}

func configPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "callvpn", "config.json")
}

func loadConfig() appConfig {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return appConfig{Instances: []instanceConfig{{}}}
	}

	// Try new format first.
	var cfg appConfig
	if err := json.Unmarshal(data, &cfg); err == nil && len(cfg.Instances) > 0 {
		return cfg
	}

	// Migrate from legacy single-instance format.
	var legacy legacyConfig
	if err := json.Unmarshal(data, &legacy); err == nil && (legacy.CallLink != "" || legacy.AuthToken != "") {
		cfg = appConfig{
			Instances: []instanceConfig{
				{CallLink: legacy.CallLink, AuthToken: legacy.AuthToken},
			},
		}
		saveConfig(cfg)
		return cfg
	}

	return appConfig{Instances: []instanceConfig{{}}}
}

func saveConfig(cfg appConfig) {
	path := configPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.Marshal(cfg)
	os.WriteFile(path, data, 0o644)
}

// instance holds per-instance runtime state.
type instance struct {
	mu        sync.Mutex
	srv       *server.Server
	cancel    context.CancelFunc
	connected bool
	logs      *logWriter
}

type appState struct {
	mu        sync.Mutex
	instances []*instance
}

func newAppState(n int) *appState {
	s := &appState{}
	for i := 0; i < n; i++ {
		s.instances = append(s.instances, &instance{logs: newLogWriter()})
	}
	return s
}

func (s *appState) getInstance(id int) *instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 0 || id >= len(s.instances) {
		return nil
	}
	return s.instances[id]
}

func (s *appState) addInstance() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instances = append(s.instances, &instance{logs: newLogWriter()})
	return len(s.instances) - 1
}

func (s *appState) removeInstance(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 0 || id >= len(s.instances) || len(s.instances) <= 1 {
		return false
	}
	inst := s.instances[id]
	inst.mu.Lock()
	if inst.connected {
		inst.mu.Unlock()
		return false
	}
	inst.mu.Unlock()
	s.instances = append(s.instances[:id], s.instances[id+1:]...)
	return true
}

func (s *appState) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.instances)
}

func (s *appState) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (s *appState) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		var cfg appConfig
		json.NewDecoder(r.Body).Decode(&cfg)
		if len(cfg.Instances) == 0 {
			cfg.Instances = []instanceConfig{{}}
		}
		saveConfig(cfg)
		json.NewEncoder(w).Encode(cfg)
		return
	}
	json.NewEncoder(w).Encode(loadConfig())
}

func (s *appState) handleStatus(w http.ResponseWriter, r *http.Request) {
	type instanceStatus struct {
		ID        int    `json:"id"`
		Connected bool   `json:"connected"`
		Sessions  []any  `json:"sessions,omitempty"`
	}

	s.mu.Lock()
	n := len(s.instances)
	instances := make([]*instance, n)
	copy(instances, s.instances)
	s.mu.Unlock()

	statuses := make([]instanceStatus, n)
	for i, inst := range instances {
		inst.mu.Lock()
		st := instanceStatus{ID: i, Connected: inst.connected}
		if inst.connected && inst.srv != nil {
			sessions := inst.srv.GetSessionsInfo()
			st.Sessions = make([]any, len(sessions))
			for j, sess := range sessions {
				st.Sessions[j] = sess
			}
		}
		inst.mu.Unlock()
		statuses[i] = st
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"instances": statuses})
}

func (s *appState) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID    int    `json:"id"`
		Link  string `json:"link"`
		Token string `json:"token"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	link := parseCallLink(req.Link)
	if link == "" {
		http.Error(w, `{"error":"empty link"}`, http.StatusBadRequest)
		return
	}

	inst := s.getInstance(req.ID)
	if inst == nil {
		http.Error(w, `{"error":"invalid instance id"}`, http.StatusBadRequest)
		return
	}

	inst.mu.Lock()
	if inst.connected {
		inst.mu.Unlock()
		http.Error(w, `{"error":"already connected"}`, http.StatusConflict)
		return
	}
	inst.mu.Unlock()

	// Save config for all instances.
	cfg := loadConfig()
	for len(cfg.Instances) <= req.ID {
		cfg.Instances = append(cfg.Instances, instanceConfig{})
	}
	cfg.Instances[req.ID] = instanceConfig{CallLink: req.Link, AuthToken: req.Token}
	saveConfig(cfg)

	logger := slog.New(slog.NewTextHandler(inst.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	authToken := req.Token
	if authToken == "" {
		authToken = os.Getenv("VPN_TOKEN")
	}

	var svc provider.Service
	if telemost.IsTelemostLink(link) {
		svc = telemost.NewService(link)
	} else {
		svc = vk.NewService(link)
	}

	srvCfg := server.Config{
		Service:   svc,
		AuthToken: authToken,
		UseTCP:    true,
		Logger:    logger,
	}

	ctx, cancel := context.WithCancel(context.Background())
	srv := server.New(srvCfg)
	srv.Start(ctx)

	inst.mu.Lock()
	inst.srv = srv
	inst.cancel = cancel
	inst.connected = true
	inst.mu.Unlock()

	// Watch for unexpected server stop.
	go func() {
		<-srv.Done()
		inst.mu.Lock()
		if inst.srv == srv {
			inst.connected = false
			inst.srv = nil
			inst.cancel = nil
		}
		inst.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (s *appState) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID int `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	inst := s.getInstance(req.ID)
	if inst == nil {
		http.Error(w, `{"error":"invalid instance id"}`, http.StatusBadRequest)
		return
	}

	inst.mu.Lock()
	cancel := inst.cancel
	srv := inst.srv
	inst.connected = false
	inst.srv = nil
	inst.cancel = nil
	inst.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if srv != nil {
		<-srv.Done()
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (s *appState) handleLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	idStr := r.URL.Query().Get("id")
	id, _ := strconv.Atoi(idStr)

	inst := s.getInstance(id)
	if inst == nil {
		http.Error(w, "invalid instance id", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send existing logs.
	for _, line := range inst.logs.allLines() {
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
	flusher.Flush()

	// Stream new logs.
	ch, unsub := inst.logs.subscribe()
	defer unsub()

	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
	}
}

func (s *appState) handleAddInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := s.addInstance()

	// Extend config.
	cfg := loadConfig()
	for len(cfg.Instances) <= id {
		cfg.Instances = append(cfg.Instances, instanceConfig{})
	}
	saveConfig(cfg)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"id": id})
}

func (s *appState) handleRemoveInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID int `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	if !s.removeInstance(req.ID) {
		http.Error(w, `{"error":"cannot remove instance"}`, http.StatusBadRequest)
		return
	}

	// Shrink config.
	cfg := loadConfig()
	if req.ID < len(cfg.Instances) {
		cfg.Instances = append(cfg.Instances[:req.ID], cfg.Instances[req.ID+1:]...)
	}
	saveConfig(cfg)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}

func main() {
	cfg := loadConfig()
	state := newAppState(len(cfg.Instances))

	mux := http.NewServeMux()
	mux.HandleFunc("/", state.handleIndex)
	mux.HandleFunc("/api/config", state.handleConfig)
	mux.HandleFunc("/api/status", state.handleStatus)
	mux.HandleFunc("/api/connect", state.handleConnect)
	mux.HandleFunc("/api/disconnect", state.handleDisconnect)
	mux.HandleFunc("/api/logs", state.handleLogs)
	mux.HandleFunc("/api/instances/add", state.handleAddInstance)
	mux.HandleFunc("/api/instances/remove", state.handleRemoveInstance)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to listen: %v\n", err)
		os.Exit(1)
	}

	url := fmt.Sprintf("http://%s", ln.Addr())
	fmt.Printf("Call VPN Server UI: %s\n", url)
	openBrowser(url)

	// Graceful shutdown on signal.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		fmt.Println("\nshutting down...")
		state.mu.Lock()
		for _, inst := range state.instances {
			inst.mu.Lock()
			if inst.cancel != nil {
				inst.cancel()
			}
			if inst.srv != nil {
				<-inst.srv.Done()
			}
			inst.mu.Unlock()
		}
		state.mu.Unlock()
		os.Exit(0)
	}()

	http.Serve(ln, mux)
}
