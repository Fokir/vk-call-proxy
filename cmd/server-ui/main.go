package main

import (
	"context"
	_"embed"
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
	"strings"
	"sync"
	"syscall"

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

// appConfig persists between restarts.
type appConfig struct {
	CallLink  string `json:"call_link"`
	AuthToken string `json:"auth_token"`
}

func configPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "callvpn", "config.json")
}

func loadConfig() appConfig {
	var cfg appConfig
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	return cfg
}

func saveConfig(cfg appConfig) {
	path := configPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.Marshal(cfg)
	os.WriteFile(path, data, 0o644)
}

type appState struct {
	mu        sync.Mutex
	srv       *server.Server
	cancel    context.CancelFunc
	connected bool
	logs      *logWriter
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
		saveConfig(cfg)
		json.NewEncoder(w).Encode(cfg)
		return
	}
	json.NewEncoder(w).Encode(loadConfig())
}

func (s *appState) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	connected := s.connected
	srv := s.srv
	s.mu.Unlock()

	resp := map[string]any{"connected": connected}
	if connected && srv != nil {
		sessions := srv.GetSessionsInfo()
		resp["sessions"] = sessions
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *appState) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Link  string `json:"link"`
		Token string `json:"token"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	link := parseCallLink(req.Link)
	if link == "" {
		http.Error(w, `{"error":"empty link"}`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.connected {
		s.mu.Unlock()
		http.Error(w, `{"error":"already connected"}`, http.StatusConflict)
		return
	}
	s.mu.Unlock()

	// Save config.
	saveConfig(appConfig{CallLink: req.Link, AuthToken: req.Token})

	logger := slog.New(slog.NewTextHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	authToken := req.Token
	if authToken == "" {
		authToken = os.Getenv("VPN_TOKEN")
	}

	cfg := server.Config{
		CallLink:  link,
		AuthToken: authToken,
		UseTCP:    true,
		Logger:    logger,
	}

	ctx, cancel := context.WithCancel(context.Background())
	srv := server.New(cfg)
	srv.Start(ctx)

	s.mu.Lock()
	s.srv = srv
	s.cancel = cancel
	s.connected = true
	s.mu.Unlock()

	// Watch for unexpected server stop.
	go func() {
		<-srv.Done()
		s.mu.Lock()
		if s.srv == srv {
			s.connected = false
			s.srv = nil
			s.cancel = nil
		}
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (s *appState) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	cancel := s.cancel
	srv := s.srv
	s.connected = false
	s.srv = nil
	s.cancel = nil
	s.mu.Unlock()

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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send existing logs.
	for _, line := range s.logs.allLines() {
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
	flusher.Flush()

	// Stream new logs.
	ch, unsub := s.logs.subscribe()
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
	state := &appState{logs: newLogWriter()}

	mux := http.NewServeMux()
	mux.HandleFunc("/", state.handleIndex)
	mux.HandleFunc("/api/config", state.handleConfig)
	mux.HandleFunc("/api/status", state.handleStatus)
	mux.HandleFunc("/api/connect", state.handleConnect)
	mux.HandleFunc("/api/disconnect", state.handleDisconnect)
	mux.HandleFunc("/api/logs", state.handleLogs)

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
		if state.cancel != nil {
			state.cancel()
		}
		if state.srv != nil {
			<-state.srv.Done()
		}
		state.mu.Unlock()
		os.Exit(0)
	}()

	http.Serve(ln, mux)
}
