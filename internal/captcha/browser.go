package captcha

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// BrowserSolver opens the captcha in a Chrome window with a built-in forward
// proxy so that VK domains are accessible even when blocked at the network level.
// It injects a postMessage listener to capture the success_token automatically.
type BrowserSolver struct{}

func NewBrowserSolver() *BrowserSolver {
	return &BrowserSolver{}
}

// callbackPage is an HTML wrapper that embeds the captcha iframe and
// listens for the success_token via postMessage, then redirects to localhost callback.
const callbackPage = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>VK Captcha</title>
<style>body{margin:0;display:flex;justify-content:center;align-items:center;height:100vh;background:#f0f2f5;font-family:sans-serif}
iframe{border:none;width:400px;height:500px;border-radius:12px;box-shadow:0 2px 8px rgba(0,0,0,0.15)}</style></head>
<body>
<iframe id="cf" src="%s"></iframe>
<script>
window.addEventListener("message",function(e){
	try{
		var d=typeof e.data==="string"?JSON.parse(e.data):e.data;
		if(d.success_token){
			window.location="/callback?token="+encodeURIComponent(d.success_token);
		}
	}catch(ex){}
});
</script>
</body></html>`

func (b *BrowserSolver) SolveCaptcha(ctx context.Context, ch *provider.CaptchaChallenge) (*provider.CaptchaResult, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	tokenCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, callbackPage, ch.RedirectURI)
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token != "" {
			select {
			case tokenCh <- token:
			default:
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><body><h2>Captcha solved. You can close this tab.</h2></body></html>`)
	})

	// Handler that supports both normal HTTP requests (captcha page, callback)
	// and CONNECT tunneling (forward proxy for HTTPS to VK domains).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handleConnect(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	defer srv.Close()

	pageURL := fmt.Sprintf("http://%s/", addr)
	proxyAddr := addr
	slog.Info("opening captcha in browser with proxy", "url", pageURL, "proxy", proxyAddr)

	if err := openChrome(pageURL, proxyAddr); err != nil {
		// Fall back to system default browser without proxy.
		slog.Warn("Chrome not found, opening default browser (VK may be unreachable)", "err", err)
		openBrowser(pageURL)
	}

	select {
	case token := <-tokenCh:
		return &provider.CaptchaResult{SuccessToken: token}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// handleConnect implements HTTP CONNECT tunneling for the forward proxy.
func handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}

	dest, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		dest.Close()
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	client, _, err := hj.Hijack()
	if err != nil {
		dest.Close()
		return
	}

	go func() {
		defer dest.Close()
		defer client.Close()
		io.Copy(dest, client)
	}()
	go func() {
		defer dest.Close()
		defer client.Close()
		io.Copy(client, dest)
	}()
}

// openChrome tries to launch Chrome with the given proxy and URL.
func openChrome(url, proxyAddr string) error {
	chromePath, err := findChrome()
	if err != nil {
		return err
	}

	args := []string{
		"--proxy-server=http://" + proxyAddr,
		"--no-first-run",
		"--disable-default-apps",
		"--user-data-dir=" + chromeUserDataDir(),
		url,
	}

	cmd := exec.Command(chromePath, args...)
	return cmd.Start()
}

// findChrome locates the Chrome executable on the system.
func findChrome() (string, error) {
	switch runtime.GOOS {
	case "windows":
		paths := []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		}
		for _, p := range paths {
			if fileExists(p) {
				return p, nil
			}
		}
		return "", fmt.Errorf("chrome not found")
	case "darwin":
		p := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if fileExists(p) {
			return p, nil
		}
		return "", fmt.Errorf("chrome not found")
	case "linux":
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium-browser", "chromium"} {
			if p, err := exec.LookPath(name); err == nil {
				return p, nil
			}
		}
		return "", fmt.Errorf("chrome not found")
	default:
		return "", fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func chromeUserDataDir() string {
	return filepath.Join(os.TempDir(), "callvpn-captcha-chrome")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "linux":
		return exec.Command("xdg-open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}
