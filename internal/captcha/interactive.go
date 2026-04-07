//go:build !android

package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// InteractiveSolver opens a visible Chrome window with the VK captcha page
// and waits for the user to solve it manually. The success_token is captured
// automatically via Chrome DevTools Protocol network event monitoring.
type InteractiveSolver struct {
	mu sync.Mutex
}

func NewInteractiveSolver() *InteractiveSolver {
	return &InteractiveSolver{}
}

func (s *InteractiveSolver) SolveCaptcha(ctx context.Context, ch *provider.CaptchaChallenge) (*provider.CaptchaResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, err := solveInteractive(ctx, ch.RedirectURI)
	if err != nil {
		return nil, err
	}
	return &provider.CaptchaResult{SuccessToken: token}, nil
}

func solveInteractive(ctx context.Context, redirectURI string) (string, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-gpu", true),
		chromedp.NoSandbox,
		chromedp.WindowSize(500, 600),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	tokenCh := make(chan string, 1)

	// Monitor network for captchaNotRobot.check response containing success_token.
	chromedp.ListenTarget(taskCtx, func(ev any) {
		resp, ok := ev.(*network.EventResponseReceived)
		if !ok || resp.Response == nil {
			return
		}
		url := resp.Response.URL
		if !strings.Contains(url, "captchaNotRobot.check") {
			return
		}
		reqID := resp.RequestID
		go func() {
			time.Sleep(200 * time.Millisecond)
			var body []byte
			if err := chromedp.Run(taskCtx, chromedp.ActionFunc(func(ctx context.Context) error {
				var err error
				body, err = network.GetResponseBody(reqID).Do(ctx)
				return err
			})); err != nil {
				slog.Debug("interactive captcha: failed to read response body", "err", err)
				return
			}
			var result struct {
				Response struct {
					SuccessToken string `json:"success_token"`
				} `json:"response"`
			}
			if err := json.Unmarshal(body, &result); err != nil {
				return
			}
			if result.Response.SuccessToken != "" {
				select {
				case tokenCh <- result.Response.SuccessToken:
				default:
				}
			}
		}()
	})

	slog.Info("opening captcha in browser — please solve it manually", "url", redirectURI)

	if err := chromedp.Run(taskCtx,
		network.Enable(),
		chromedp.Navigate(redirectURI),
	); err != nil {
		return "", fmt.Errorf("chromedp navigate: %w", err)
	}

	select {
	case token := <-tokenCh:
		slog.Info("captcha solved", "token_len", len(token))
		return token, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
