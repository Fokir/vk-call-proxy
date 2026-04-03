package captcha

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"runtime"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// BrowserSolver opens the captcha redirect_uri in the user's system browser
// and listens on a local HTTP server for the success_token callback.
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

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	url := fmt.Sprintf("http://%s/", addr)
	slog.Info("opening captcha in system browser", "url", url)
	if err := openBrowser(url); err != nil {
		slog.Warn("failed to open browser, please open manually", "url", url, "err", err)
	}

	select {
	case token := <-tokenCh:
		return &provider.CaptchaResult{SuccessToken: token}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
