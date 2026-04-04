//go:build !android

package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// ChromedpSolver solves VK captchas using a headless Chrome browser.
// Only one browser instance runs at a time (guarded by mutex).
type ChromedpSolver struct {
	mu sync.Mutex
}

// NewChromedpSolver creates a new ChromedpSolver.
func NewChromedpSolver() *ChromedpSolver {
	return &ChromedpSolver{}
}

// SolveCaptcha implements provider.CaptchaSolver.
func (s *ChromedpSolver) SolveCaptcha(ctx context.Context, ch *provider.CaptchaChallenge) (*provider.CaptchaResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, err := SolveCaptchaURI(ctx, ch.RedirectURI)
	if err != nil {
		return nil, err
	}
	return &provider.CaptchaResult{SuccessToken: token}, nil
}

// SolveCaptchaURI opens redirectURI in headless Chrome, simulates mouse
// movement, and intercepts the captchaNotRobot.check XHR response to
// extract the success_token. It is exported so that external callers
// (e.g. captcha-service) can use it directly with their own concurrency.
// Headless controls whether Chrome runs in headless mode.
// Set to false for debugging (shows browser window).
var Headless = true

func SolveCaptchaURI(ctx context.Context, redirectURI string) (string, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", Headless),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-gpu", true),
		chromedp.NoSandbox,
		chromedp.WindowSize(1280, 800),
		chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	tokenCh := make(chan string, 1)

	// Listen for network responses containing the captcha check result.
	chromedp.ListenTarget(taskCtx, func(ev any) {
		resp, ok := ev.(*network.EventResponseReceived)
		if !ok {
			return
		}
		if resp.Response == nil {
			return
		}
		url := resp.Response.URL
		// Log ALL captcha-related API calls for debugging.
		if strings.Contains(url, "captchaNotRobot") || strings.Contains(url, "api.vk") {
			slog.Info("captcha network", "url", url, "status", resp.Response.Status)
		}
		if !strings.Contains(url, "captchaNotRobot.check") {
			return
		}
		reqID := resp.RequestID
		// Read body in a goroutine. Use allocCtx (longer-lived) instead of taskCtx
		// to avoid "invalid context" race when taskCtx is cancelled.
		go func() {
			// Small delay to let the response body arrive.
			time.Sleep(200 * time.Millisecond)
			var body []byte
			if err := chromedp.Run(taskCtx, chromedp.ActionFunc(func(ctx context.Context) error {
				var err error
				body, err = network.GetResponseBody(reqID).Do(ctx)
				return err
			})); err != nil {
				slog.Debug("captcha: failed to read response body", "err", err)
				return
			}
			slog.Info("captchaNotRobot.check response", "body", string(body))
			var result struct {
				Response struct {
					Status       string `json:"status"`
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
			} else if result.Response.Status == "BOT" {
				slog.Warn("captcha: VK detected bot")
				select {
				case tokenCh <- "": // signal failure
				default:
				}
			}
		}()
	})

	// Inject anti-detection script BEFORE any page JS runs.
	if err := chromedp.Run(taskCtx,
		network.Enable(),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(stealthJS).Do(ctx)
			return err
		}),
		chromedp.Navigate(redirectURI),
		chromedp.WaitReady("body"),
		chromedp.Sleep(3*time.Second),
	); err != nil {
		return "", fmt.Errorf("chromedp run: %w", err)
	}

	// Dump page structure for debugging.
	var debugHTML string
	if err := chromedp.Run(taskCtx, chromedp.Evaluate(
		`(function(){
			var els = document.querySelectorAll('input, button, label, [role="checkbox"], [class*="heck"], [class*="aptcha"]');
			var info = [];
			els.forEach(function(el){
				var r = el.getBoundingClientRect();
				info.push(el.tagName + ' class=' + el.className + ' type=' + el.type + ' rect=' + Math.round(r.x) + ',' + Math.round(r.y) + ',' + Math.round(r.width) + 'x' + Math.round(r.height));
			});
			return info.join('\\n');
		})()`,
		&debugHTML,
	)); err == nil && debugHTML != "" {
		slog.Info("captcha page elements", "elements", debugHTML)
	}

	// Try to find and click the captcha checkbox, with mouse movement leading to it.
	for attempt := 0; attempt < 3; attempt++ {
		if err := chromedp.Run(taskCtx, clickCaptchaCheckbox()); err != nil {
			slog.Info("captcha click attempt failed", "attempt", attempt, "err", err)
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			continue
		}
		slog.Info("captcha click sent", "attempt", attempt)

		// After clicking, keep moving mouse — VK JS collects sensor data.
		if err := chromedp.Run(taskCtx, postClickMouseMovement()); err != nil {
			slog.Debug("post-click mouse movement failed", "err", err)
		}

		// Wait for token after clicking + mouse movement.
		select {
		case token := <-tokenCh:
			if token == "" {
				return "", fmt.Errorf("VK detected bot (status=BOT)")
			}
			return token, nil
		case <-time.After(10 * time.Second):
			// Token not arrived yet, try clicking again.
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	// Final wait for token.
	select {
	case token := <-tokenCh:
		if token == "" {
			return "", fmt.Errorf("VK detected bot (status=BOT)")
		}
		return token, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// postClickMouseMovement generates natural mouse movement after clicking
// the checkbox. VK captcha JS collects cursor sensor data for several seconds
// before sending captchaNotRobot.check.
func postClickMouseMovement() chromedp.ActionFunc {
	return func(ctx context.Context) error {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		x := 600.0 + r.Float64()*50
		y := 400.0 + r.Float64()*30

		// Move mouse naturally for ~5 seconds.
		for i := 0; i < 20; i++ {
			x += (r.Float64() - 0.5) * 40
			y += (r.Float64() - 0.5) * 20
			if err := input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx); err != nil {
				return err
			}
			delay := time.Duration(200+r.Intn(300)) * time.Millisecond
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
}

// clickCaptchaCheckbox finds the captcha checkbox on the page,
// moves the mouse toward it with natural movement, and clicks via DOM.
func clickCaptchaCheckbox() chromedp.ActionFunc {
	return func(ctx context.Context) error {
		// VK captcha checkbox selectors — prefer the visible LABEL over hidden INPUT.
		selectors := []string{
			`[class*="Checkbox-module__Checkbox"]`,
			`label[class*="Checkbox"]`,
			`label`,
			`[role="checkbox"]`,
			`input[type="checkbox"]`,
		}

		// Move mouse naturally first (VK collects cursor data before click).
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		x0 := 300.0 + r.Float64()*200
		y0 := 200.0 + r.Float64()*100
		for i := 0; i < 8; i++ {
			x0 += (r.Float64() - 0.3) * 40
			y0 += (r.Float64() - 0.3) * 20
			input.DispatchMouseEvent(input.MouseMoved, x0, y0).Do(ctx)
			time.Sleep(time.Duration(60+r.Intn(100)) * time.Millisecond)
		}

		// Click via chromedp.Click (DOM click — same as human click).
		for _, sel := range selectors {
			if err := chromedp.Click(sel, chromedp.NodeVisible).Do(ctx); err == nil {
				slog.Info("captcha checkbox clicked", "selector", sel)
				return nil
			}
		}

		return fmt.Errorf("no clickable checkbox found")
	}
}
